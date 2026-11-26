import contextlib
import io
import json
from pathlib import Path
from types import SimpleNamespace
import tempfile
import unittest
from unittest.mock import patch

import benchmark
import paired_experiment


class FakeStream:
    def __enter__(self):
        return self

    def __exit__(self, *_args):
        return False

    def __iter__(self):
        return iter([
            b'data: {"choices":[{"text":"A","finish_reason":null}],"system_fingerprint":"vllm-test"}\n',
            b'data: {"choices":[{"text":"B","finish_reason":"length"}]}\n',
            b'data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2},"metrics":{"time_to_first_token_ms":12.3}}\n',
            b'data: [DONE]\n',
        ])


class ExperimentTest(unittest.TestCase):
    def test_benchmark_saves_request_timings_and_usage(self):
        with tempfile.TemporaryDirectory() as directory:
            details = Path(directory) / "requests.jsonl"

            def open_stream(request, timeout):
                body = json.loads(request.data)
                self.assertEqual(body["stream_options"], {"include_usage": True})
                self.assertGreater(timeout, 0)
                return FakeStream()

            output = io.StringIO()
            argv = ["benchmark.py", "--model", "test-model", "--requests", "3",
                    "--concurrency", "2", "--details-file", str(details)]
            with patch("benchmark.urllib.request.urlopen", side_effect=open_stream), \
                    patch("sys.argv", argv), contextlib.redirect_stdout(output):
                benchmark.main()

            summary = json.loads(output.getvalue())
            records = [json.loads(line) for line in details.read_text().splitlines()]
            self.assertEqual(summary["completed"], 3)
            self.assertEqual(summary["completion_tokens"], 6)
            self.assertEqual(summary["server_ttft_seconds"]["0.95"], 0.0123)
            self.assertEqual({r["request_id"] for r in records}, {0, 1, 2})
            self.assertTrue(all(r["ttft_seconds"] >= 0 for r in records))
            self.assertTrue(all(r["finish_reason"] == "length" for r in records))
            self.assertTrue(all(r["system_fingerprint"] == "vllm-test" for r in records))

    def test_weight_restoration_preserves_external_change(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "cpu.weight"
            path.write_text("200\n")
            paired_experiment.restore_weight(path, 100, 200)
            self.assertEqual(path.read_text(), "100\n")

            path.write_text("300\n")
            with self.assertRaisesRegex(RuntimeError, "changed externally"):
                paired_experiment.restore_weight(path, 100, 200)
            self.assertEqual(path.read_text(), "300\n")

    def test_agent_samples_must_be_valid_and_observing(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "agent.jsonl"
            path.write_text(json.dumps({"report": {
                "enforcement_enabled": False, "observation": {"valid": True}}}) + "\n")
            self.assertEqual(paired_experiment.valid_agent_samples(path), 1)

            path.write_text(json.dumps({"report": {
                "enforcement_enabled": True, "observation": {"valid": True}}}) + "\n")
            with self.assertRaisesRegex(RuntimeError, "enabled enforcement"):
                paired_experiment.valid_agent_samples(path)

    def test_failed_arm_restores_weight_and_stops_stress(self):
        with tempfile.TemporaryDirectory() as directory:
            weight = Path(directory) / "cpu.weight"
            weight.write_text("100\n")
            identity = {"start_time": "1", "membership": "/test",
                        "cgroup": directory, "weight_path": str(weight)}
            args = SimpleNamespace(pid=123, model="test", url="http://localhost:8000",
                                   agent_url="http://localhost:8081/status", prompt="test",
                                   pairs=1, requests=2, concurrency=1, max_tokens=2,
                                   timeout=1, warmup_requests=0, stress_workers=1,
                                   stress_shares=8192, stress_seconds=60,
                                   min_stress_cpu=100, stress_image="ubuntu:latest",
                                   expected_weight=100, seed=1,
                                   output_dir=Path(directory) / "results", dry_run=False)

            def write_sample(_stop, path, _url, _interval):
                path.write_text(json.dumps({"report": {
                    "enforcement_enabled": False, "observation": {"valid": True}}}) + "\n")

            def failed_benchmark(_args, _output_dir, _requests):
                self.assertEqual(weight.read_text(), "200\n")
                raise RuntimeError("injected failure")

            with patch("paired_experiment.parse_args", return_value=args), \
                    patch("paired_experiment.schedule_for", return_value=[["boost", "control"]]), \
                    patch("paired_experiment.target_identity", return_value=identity), \
                    patch("paired_experiment.os.geteuid", return_value=0), \
                    patch("paired_experiment.check_agent_report"), \
                    patch("paired_experiment.checked_command", return_value="container-id"), \
                    patch("paired_experiment.stress_running", return_value=True), \
                    patch("paired_experiment.stress_cpu", return_value=398), \
                    patch("paired_experiment.sample_agent", side_effect=write_sample), \
                    patch("paired_experiment.run_benchmark", side_effect=failed_benchmark), \
                    patch("paired_experiment.time.sleep"), \
                    patch("paired_experiment.subprocess.run") as docker_stop:
                with self.assertRaisesRegex(RuntimeError, "injected failure"):
                    paired_experiment.main()
            self.assertEqual(weight.read_text(), "100\n")
            self.assertEqual(docker_stop.call_args.args[0][:2], ["docker", "stop"])

    def test_completed_pair_writes_comparison_and_restores_weight(self):
        with tempfile.TemporaryDirectory() as directory:
            weight = Path(directory) / "cpu.weight"
            weight.write_text("100\n")
            identity = {"start_time": "1", "membership": "/test",
                        "cgroup": directory, "weight_path": str(weight)}
            output_dir = Path(directory) / "results"
            args = SimpleNamespace(pid=123, model="test", url="http://localhost:8000",
                                   agent_url="http://localhost:8081/status", prompt="test",
                                   pairs=1, requests=2, concurrency=1, max_tokens=2,
                                   timeout=1, warmup_requests=0, stress_workers=1,
                                   stress_shares=8192, stress_seconds=60,
                                   min_stress_cpu=100, stress_image="ubuntu:latest",
                                   expected_weight=100, seed=1,
                                   output_dir=output_dir, dry_run=False)

            def write_sample(_stop, path, _url, _interval):
                path.write_text(json.dumps({"report": {
                    "enforcement_enabled": False, "observation": {"valid": True}}}) + "\n")

            def fake_benchmark(_args, arm_dir, _requests):
                arm_dir.mkdir()
                p95 = 0.04 if weight.read_text() == "100\n" else 0.03
                return {"ttft_seconds": {"0.95": p95, "0.99": 0.05},
                        "requests_per_second": 1.3}

            with patch("paired_experiment.parse_args", return_value=args), \
                    patch("paired_experiment.schedule_for", return_value=[["control", "boost"]]), \
                    patch("paired_experiment.target_identity", return_value=identity), \
                    patch("paired_experiment.os.geteuid", return_value=0), \
                    patch("paired_experiment.check_agent_report"), \
                    patch("paired_experiment.checked_command", return_value="container-id"), \
                    patch("paired_experiment.stress_running", return_value=True), \
                    patch("paired_experiment.stress_cpu", return_value=398), \
                    patch("paired_experiment.sample_agent", side_effect=write_sample), \
                    patch("paired_experiment.run_benchmark", side_effect=fake_benchmark), \
                    patch("paired_experiment.time.sleep"), \
                    patch("paired_experiment.subprocess.run"):
                with contextlib.redirect_stdout(io.StringIO()):
                    paired_experiment.main()
            comparison = json.loads((output_dir / "comparison.json").read_text())
            self.assertAlmostEqual(comparison["median_p95_reduction_fraction"], 0.25)
            self.assertEqual(comparison["boost_faster_pairs"], 1)
            self.assertEqual(weight.read_text(), "100\n")


if __name__ == "__main__":
    unittest.main()
