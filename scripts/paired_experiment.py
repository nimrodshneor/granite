#!/usr/bin/env python3
"""Run paired CPU-weight trials against a local vLLM server on a Linux host.

Run with sudo: Docker and the target cgroup's cpu.weight require host access.
The agent can keep running in observation mode during this manual intervention.
"""

import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import platform
import random
import signal
import statistics
import subprocess
import sys
import threading
import time
import urllib.request


CGROUP_ROOT = Path("/sys/fs/cgroup")
BENCHMARK = Path(__file__).with_name("benchmark.py")


def utc_now():
    return datetime.now(timezone.utc).isoformat()


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2) + "\n", encoding="utf-8")


def target_identity(pid):
    stat = Path("/proc") / str(pid) / "stat"
    raw = stat.read_text(encoding="utf-8")
    end = raw.rfind(")")
    if end < 0 or len(raw[end + 1:].split()) <= 19:
        raise RuntimeError("invalid target process stat")
    start_time = raw[end + 1:].split()[19]
    lines = (Path("/proc") / str(pid) / "cgroup").read_text(encoding="utf-8").splitlines()
    memberships = [line[3:] for line in lines if line.startswith("0::")]
    if len(memberships) != 1 or not memberships[0].startswith("/"):
        raise RuntimeError("target must have one cgroup v2 membership")
    relative = Path(memberships[0].lstrip("/"))
    if not relative.parts or ".." in relative.parts:
        raise RuntimeError("target must be in a non-root cgroup")
    cgroup = (CGROUP_ROOT / relative).resolve(strict=True)
    if os.path.commonpath((str(CGROUP_ROOT), str(cgroup))) != str(CGROUP_ROOT):
        raise RuntimeError("target cgroup escapes /sys/fs/cgroup")
    weight = cgroup / "cpu.weight"
    if not weight.is_file():
        raise RuntimeError("target cgroup has no cpu.weight")
    return {"start_time": start_time, "membership": memberships[0],
            "cgroup": str(cgroup), "weight_path": str(weight)}


def schedule_for(pairs, seed):
    first = random.Random(seed).randrange(2)
    return [["control", "boost"] if (first + i) % 2 == 0 else ["boost", "control"]
            for i in range(pairs)]


def checked_command(argv):
    result = subprocess.run(argv, text=True, stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, check=False)
    if result.returncode:
        raise RuntimeError("%s failed: %s" % (" ".join(argv[:2]), result.stderr.strip()))
    return result.stdout.strip()


def optional_command(argv):
    try:
        result = subprocess.run(argv, text=True, stdout=subprocess.PIPE,
                                stderr=subprocess.DEVNULL, check=False)
    except OSError:
        return None
    return result.stdout.strip() if result.returncode == 0 else None


def stress_running(name):
    result = subprocess.run(["docker", "inspect", "--format", "{{.State.Running}}", name],
                            text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            check=False)
    return result.returncode == 0 and result.stdout.strip() == "true"


def stress_cpu(name):
    value = checked_command(["docker", "stats", "--no-stream", "--format",
                             "{{.CPUPerc}}", name])
    return float(value.rstrip("%"))


def restore_weight(path, original, boosted):
    current = int(path.read_text(encoding="utf-8").strip())
    if current not in (original, boosted):
        raise RuntimeError("cpu.weight changed externally; leaving it untouched")
    if current != original:
        path.write_text("%d\n" % original, encoding="utf-8")


def sample_agent(stop, path, url, interval):
    with path.open("w", encoding="utf-8") as output:
        while not stop.is_set():
            entry = {"captured_at": utc_now()}
            try:
                with urllib.request.urlopen(url, timeout=2) as response:
                    entry["report"] = json.load(response)
            except Exception as exc:
                entry["error"] = str(exc)
            output.write(json.dumps(entry) + "\n")
            output.flush()
            stop.wait(interval)


def check_agent_report(url):
    with urllib.request.urlopen(url, timeout=3) as response:
        report = json.load(response)
    if report.get("enforcement_enabled"):
        raise RuntimeError("agent must run in observation mode during manual trials")
    return report


def valid_agent_samples(path):
    count = 0
    for line in path.read_text(encoding="utf-8").splitlines():
        report = json.loads(line).get("report")
        if report is None:
            continue
        if report.get("enforcement_enabled"):
            raise RuntimeError("agent enabled enforcement during a manual trial")
        if report.get("observation", {}).get("valid"):
            count += 1
    if count == 0:
        raise RuntimeError("no valid agent telemetry during an arm; inspect %s" % path)
    return count


def run_benchmark(args, output_dir, requests):
    output_dir.mkdir()
    command = [sys.executable, str(BENCHMARK), "--url", args.url,
               "--model", args.model, "--prompt", args.prompt,
               "--requests", str(requests), "--concurrency", str(args.concurrency),
               "--max-tokens", str(args.max_tokens), "--timeout", str(args.timeout),
               "--details-file", str(output_dir / "requests.jsonl")]
    summary_path = output_dir / "summary.json"
    stderr_path = output_dir / "benchmark.stderr"
    with summary_path.open("w", encoding="utf-8") as summary, \
            stderr_path.open("w", encoding="utf-8") as stderr:
        process = subprocess.Popen(command, stdout=summary, stderr=stderr)
        try:
            return_code = process.wait()
        except BaseException:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
            raise
    if return_code:
        raise RuntimeError("benchmark failed; inspect %s and %s" %
                           (summary_path, stderr_path))
    result = json.loads(summary_path.read_text(encoding="utf-8"))
    validate_summary(result, requests, args.require_server_metrics)
    return result


def validate_summary(result, requests, require_server_metrics):
    if result["completed"] != requests or result["errors"]:
        raise RuntimeError("benchmark did not complete all requests")
    if require_server_metrics and not (result.get("server_ttft_seconds") or {}).get("0.95"):
        raise RuntimeError("server-side TTFT is missing; restart vLLM with "
                           "--enable-per-request-metrics before this experiment")


def compare(arms, pairs):
    comparisons = []
    for pair in range(1, pairs + 1):
        by_weight = {arm["weight_name"]: arm["summary"] for arm in arms
                     if arm["pair"] == pair}
        control, boost = by_weight["control"], by_weight["boost"]
        control_p95 = control["ttft_seconds"]["0.95"]
        boost_p95 = boost["ttft_seconds"]["0.95"]
        comparisons.append({
            "pair": pair,
            "control_p95_seconds": control_p95,
            "boost_p95_seconds": boost_p95,
            "p95_reduction_fraction": 1 - boost_p95 / control_p95,
            "control_p99_seconds": control["ttft_seconds"]["0.99"],
            "boost_p99_seconds": boost["ttft_seconds"]["0.99"],
            "control_server_p95_seconds": (control.get("server_ttft_seconds") or {}).get("0.95"),
            "boost_server_p95_seconds": (boost.get("server_ttft_seconds") or {}).get("0.95"),
            "control_requests_per_second": control["requests_per_second"],
            "boost_requests_per_second": boost["requests_per_second"],
        })
    reductions = [pair["p95_reduction_fraction"] for pair in comparisons]
    server_reductions = [1 - pair["boost_server_p95_seconds"] / pair["control_server_p95_seconds"]
                         for pair in comparisons if pair["control_server_p95_seconds"]
                         and pair["boost_server_p95_seconds"]]
    return {"pairs": comparisons,
            "median_p95_reduction_fraction": statistics.median(reductions),
            "boost_faster_pairs": sum(value > 0 for value in reductions),
            "median_server_p95_reduction_fraction": (statistics.median(server_reductions)
                                                     if len(server_reductions) == pairs else None),
            "server_boost_faster_pairs": (sum(value > 0 for value in server_reductions)
                                          if len(server_reductions) == pairs else None),
            "pair_count": pairs}


def parse_args():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--pid", type=int, required=True, help="vLLM engine host PID")
    parser.add_argument("--model", required=True)
    parser.add_argument("--url", default="http://127.0.0.1:8000")
    parser.add_argument("--agent-url", default="http://127.0.0.1:8081/status")
    parser.add_argument("--prompt", default="Explain how CPU scheduling affects inference latency.")
    parser.add_argument("--pairs", type=int, default=5)
    parser.add_argument("--requests", type=int, default=200, help="Requests per arm")
    parser.add_argument("--concurrency", type=int, default=1)
    parser.add_argument("--max-tokens", type=int, default=128)
    parser.add_argument("--timeout", type=float, default=120, help="Per-request timeout in seconds")
    parser.add_argument("--warmup-requests", type=int, default=5)
    parser.add_argument("--stress-workers", type=int, default=4)
    parser.add_argument("--stress-shares", type=int, default=8192)
    parser.add_argument("--stress-seconds", type=int, default=3600)
    parser.add_argument("--min-stress-cpu", type=float, default=100,
                        help="Minimum contender CPU percent before and after each arm")
    parser.add_argument("--stress-image", default="ubuntu:latest")
    parser.add_argument("--expected-weight", type=int, default=100)
    parser.add_argument("--require-server-metrics", action="store_true",
                        help="Abort during warmup if vLLM omits per-request server TTFT")
    parser.add_argument("--seed", type=int)
    parser.add_argument("--output-dir", type=Path)
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()
    values = (args.pid, args.pairs, args.requests, args.concurrency,
              args.max_tokens, args.timeout, args.stress_workers, args.stress_seconds)
    if (min(values) <= 0 or args.warmup_requests < 0 or args.stress_shares < 2
            or args.min_stress_cpu < 0):
        parser.error("counts, PID, timeout, and stress duration must be positive")
    if args.expected_weight < 1 or args.expected_weight >= 10000:
        parser.error("--expected-weight must be between 1 and 9999")
    return args


def main():
    args = parse_args()
    identity = target_identity(args.pid)
    weight_path = Path(identity["weight_path"])
    original = int(weight_path.read_text(encoding="utf-8").strip())
    if original != args.expected_weight:
        raise RuntimeError("cpu.weight is %d, expected %d" % (original, args.expected_weight))
    boosted = min(original * 2, 10000)
    seed = args.seed if args.seed is not None else random.SystemRandom().randrange(2**32)
    schedule = schedule_for(args.pairs, seed)
    output_dir = args.output_dir or Path("experiment-results") / datetime.now(timezone.utc).strftime("%Y%m%d-%H%M%S")
    stress_name = "granite-exp-%d-%d" % (args.pid, int(time.time()))
    loop = "for i in {1..%d}; do (while :; do :; done) & done; wait" % args.stress_workers
    stress_command = ["docker", "run", "-d", "--rm", "--pull=never",
                      "--name", stress_name, "--cpu-shares", str(args.stress_shares),
                      args.stress_image, "timeout", "%ds" % args.stress_seconds,
                      "bash", "-c", loop]
    manifest = {"started_at": utc_now(), "model": args.model, "url": args.url,
                "pid": args.pid, "target": identity, "control_weight": original,
                "boost_weight": boosted, "pairs": args.pairs, "requests_per_arm": args.requests,
                "concurrency": args.concurrency, "max_tokens": args.max_tokens,
                "prompt": args.prompt, "seed": seed, "schedule": schedule,
                "require_server_metrics": args.require_server_metrics,
                "stress_container": stress_name, "stress_command": stress_command,
                "min_stress_cpu_percent": args.min_stress_cpu,
                "host": platform.uname()._asdict(), "python": sys.version,
                "granite_revision": optional_command(
                    ["git", "-c", "safe.directory=" + str(Path.cwd()), "rev-parse", "HEAD"]),
                "gpu": optional_command(["nvidia-smi", "--query-gpu=name,driver_version",
                                         "--format=csv,noheader"])}
    if args.dry_run:
        print(json.dumps(manifest, indent=2))
        return
    if os.geteuid() != 0:
        raise RuntimeError("run with sudo to manage Docker and cpu.weight")
    check_agent_report(args.agent_url)
    output_dir.mkdir(parents=True, exist_ok=False)
    write_json(output_dir / "manifest.json", manifest)
    (output_dir / "recovery.txt").write_text(
        "If the process is killed before cleanup, restore the original weight and stop stress:\n"
        "sudo sh -c 'printf \"%d\\n\" > %s'\n" % (original, weight_path) +
        "sudo docker stop %s\n" % stress_name, encoding="utf-8")
    arms = []
    stress_started = False
    try:
        if args.warmup_requests:
            print("Warming up vLLM with %d requests" % args.warmup_requests, flush=True)
            run_benchmark(args, output_dir / "warmup", args.warmup_requests)
        print("Starting bounded CPU contention: %s" % stress_name, flush=True)
        checked_command(stress_command)
        stress_started = True
        time.sleep(2)
        if not stress_running(stress_name):
            raise RuntimeError("stress container exited after startup")
        expected_current_weight = original
        for pair_number, order in enumerate(schedule, 1):
            for weight_name in order:
                if target_identity(args.pid) != identity:
                    raise RuntimeError("target process identity or cgroup changed")
                if not stress_running(stress_name):
                    raise RuntimeError("stress container exited; aborting comparison")
                if int(weight_path.read_text(encoding="utf-8").strip()) != expected_current_weight:
                    raise RuntimeError("cpu.weight changed externally")
                weight = original if weight_name == "control" else boosted
                weight_path.write_text("%d\n" % weight, encoding="utf-8")
                if int(weight_path.read_text(encoding="utf-8").strip()) != weight:
                    raise RuntimeError("cpu.weight did not accept requested value")
                expected_current_weight = weight
                arm_dir = output_dir / ("pair-%02d-%s" % (pair_number, weight_name))
                stop = threading.Event()
                monitor = threading.Thread(target=sample_agent,
                                           args=(stop, arm_dir / "agent.jsonl", args.agent_url, 5),
                                           daemon=True)
                cpu_before = stress_cpu(stress_name)
                if cpu_before < args.min_stress_cpu:
                    raise RuntimeError("stress CPU fell below %.1f%% before an arm" % args.min_stress_cpu)
                print("Pair %d/%d: %s (cpu.weight=%d, stress=%.1f%%)" %
                      (pair_number, args.pairs, weight_name, weight, cpu_before), flush=True)
                started_at = utc_now()
                arm_dir.mkdir()
                monitor.start()
                try:
                    summary = run_benchmark(args, arm_dir / "benchmark", args.requests)
                finally:
                    stop.set()
                    monitor.join(timeout=3)
                valid_samples = valid_agent_samples(arm_dir / "agent.jsonl")
                if not stress_running(stress_name):
                    raise RuntimeError("stress container exited during an arm; results are invalid")
                if target_identity(args.pid) != identity:
                    raise RuntimeError("target process identity or cgroup changed during an arm")
                if int(weight_path.read_text(encoding="utf-8").strip()) != weight:
                    raise RuntimeError("cpu.weight changed during an arm")
                cpu_after = stress_cpu(stress_name)
                if cpu_after < args.min_stress_cpu:
                    raise RuntimeError("stress CPU fell below %.1f%% after an arm" % args.min_stress_cpu)
                arm = {"pair": pair_number, "weight_name": weight_name, "weight": weight,
                       "started_at": started_at, "finished_at": utc_now(),
                       "valid_agent_samples": valid_samples,
                       "stress_cpu_percent_before": cpu_before,
                       "stress_cpu_percent_after": cpu_after, "summary": summary}
                write_json(arm_dir / "arm.json", arm)
                arms.append(arm)
                write_json(output_dir / "completed_arms.json", arms)
        comparison = compare(arms, args.pairs)
        write_json(output_dir / "comparison.json", comparison)
        print(json.dumps(comparison, indent=2))
        print("Results saved in %s" % output_dir, flush=True)
    finally:
        try:
            restore_weight(weight_path, original, boosted)
        finally:
            if stress_started:
                subprocess.run(["docker", "stop", "--time", "2", stress_name],
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                               check=False)


if __name__ == "__main__":
    def interrupt(signum, _frame):
        raise KeyboardInterrupt("received signal %d" % signum)

    signal.signal(signal.SIGTERM, interrupt)
    if hasattr(signal, "SIGHUP"):
        signal.signal(signal.SIGHUP, interrupt)
    try:
        main()
    except (KeyboardInterrupt, OSError, RuntimeError, ValueError) as exc:
        print("experiment failed: %s" % exc, file=sys.stderr)
        sys.exit(1)
