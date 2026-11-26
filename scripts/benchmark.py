#!/usr/bin/env python3
"""Small repeatable streaming inference load generator; Python standard library only.

Reports per-request TTFT and streamed-content chunk gaps, not true token ITL.
Use the vLLM histograms for token-level inter-token latency.
"""
import argparse
import concurrent.futures
import json
import math
from pathlib import Path
import time
import urllib.request


def request(args, request_id):
    body = json.dumps({"model": args.model, "prompt": args.prompt,
                       "max_tokens": args.max_tokens, "temperature": 0,
                       "stream": True,
                       "stream_options": {"include_usage": True}}).encode()
    req = urllib.request.Request(args.url.rstrip("/") + "/v1/completions", body,
                                 {"Content-Type": "application/json"})
    start, first, previous, gaps = time.monotonic(), None, None, []
    usage, server_metrics, finish_reason, fingerprint = None, None, None, None
    result = {"request_id": request_id}
    try:
        with urllib.request.urlopen(req, timeout=args.timeout) as response:
            for line in response:
                if not line.startswith(b"data: "):
                    continue
                data = line[6:].strip()
                if data == b"[DONE]":
                    break
                event = json.loads(data)
                if "error" in event:
                    raise RuntimeError(event["error"])
                fingerprint = event.get("system_fingerprint") or fingerprint
                if isinstance(event.get("usage"), dict):
                    usage = event["usage"]
                if isinstance(event.get("metrics"), dict):
                    server_metrics = event["metrics"]
                for choice in event.get("choices", []):
                    finish_reason = choice.get("finish_reason") or finish_reason
                    if not choice.get("text"):
                        continue
                    now = time.monotonic()
                    if first is None:
                        first = now - start
                    if previous is not None:
                        gaps.append(now - previous)
                    previous = now
        if first is None:
            raise RuntimeError("stream returned no content")
        result.update({"ttft_seconds": first,
                       "content_chunks": len(gaps) + 1,
                       "chunk_gap_sum_seconds": sum(gaps),
                       "chunk_gap_count": len(gaps),
                       "completion_tokens": usage.get("completion_tokens") if usage else None,
                       "prompt_tokens": usage.get("prompt_tokens") if usage else None,
                       "finish_reason": finish_reason,
                       "system_fingerprint": fingerprint})
        if server_metrics and isinstance(server_metrics.get("time_to_first_token_ms"), (int, float)):
            result["server_ttft_seconds"] = server_metrics["time_to_first_token_ms"] / 1000
    except Exception as exc:
        result["error"] = str(exc)
    result["elapsed_seconds"] = time.monotonic() - start
    return result


def percentile(values, q):
    return sorted(values)[max(0, math.ceil(len(values) * q) - 1)] if values else None


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--url", default="http://127.0.0.1:8000")
    p.add_argument("--model", required=True)
    p.add_argument("--requests", type=int, default=100)
    p.add_argument("--concurrency", type=int, default=8)
    p.add_argument("--max-tokens", type=int, default=128)
    p.add_argument("--prompt", default="Explain how CPU scheduling affects inference latency.")
    p.add_argument("--timeout", type=float, default=120)
    p.add_argument("--details-file", help="Write one JSON record per request to this JSONL file")
    args = p.parse_args()
    if min(args.requests, args.concurrency, args.max_tokens, args.timeout) <= 0:
        p.error("request count, concurrency, max tokens, and timeout must be positive")
    start, results, errors = time.monotonic(), [], []
    details = None
    if args.details_file:
        Path(args.details_file).parent.mkdir(parents=True, exist_ok=True)
        details = open(args.details_file, "w", encoding="utf-8")
    try:
        with concurrent.futures.ThreadPoolExecutor(max_workers=args.concurrency) as pool:
            futures = [pool.submit(request, args, i) for i in range(args.requests)]
            for f in concurrent.futures.as_completed(futures):
                result = f.result()
                results.append(result)
                if "error" in result:
                    errors.append(result["error"])
                if details:
                    details.write(json.dumps(result) + "\n")
                    details.flush()
    finally:
        if details:
            details.close()
    elapsed = time.monotonic() - start
    completed = [r for r in results if "error" not in r]
    ttft = [r["ttft_seconds"] for r in completed]
    server_ttft = [r["server_ttft_seconds"] for r in completed if "server_ttft_seconds" in r]
    gap_count = sum(r["chunk_gap_count"] for r in completed)
    gap_sum = sum(r["chunk_gap_sum_seconds"] for r in completed)
    tokens = [r["completion_tokens"] for r in completed]
    token_total = sum(tokens) if tokens and all(isinstance(t, int) for t in tokens) else None
    print(json.dumps({"requests": args.requests, "concurrency": args.concurrency,
                      "max_tokens": args.max_tokens, "completed": len(completed),
                      "errors": errors, "elapsed_seconds": elapsed,
                      "requests_per_second": len(completed) / elapsed,
                      "ttft_seconds": {str(q): percentile(ttft, q) for q in [.5, .95, .99]},
                      "server_ttft_seconds": ({str(q): percentile(server_ttft, q)
                                               for q in [.5, .95, .99]}
                                              if completed and len(server_ttft) == len(completed) else None),
                      "mean_content_chunk_gap_seconds": gap_sum / gap_count if gap_count else None,
                      "completion_tokens": token_total,
                      "output_tokens_per_second": token_total / elapsed if token_total is not None else None},
                     indent=2))
    if errors:
        raise SystemExit(1)


if __name__ == "__main__":
    main()
