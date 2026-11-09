#!/usr/bin/env python3
"""Small repeatable streaming inference load generator; Python standard library only.

Reports per-request TTFT and streamed-content chunk gaps, not true token ITL.
Use the vLLM histograms for token-level inter-token latency.
"""
import argparse
import concurrent.futures
import json
import math
import statistics
import time
import urllib.request


def request(args):
    body = json.dumps({"model": args.model, "prompt": args.prompt,
                       "max_tokens": args.max_tokens, "temperature": 0,
                       "stream": True}).encode()
    req = urllib.request.Request(args.url.rstrip("/") + "/v1/completions", body,
                                 {"Content-Type": "application/json"})
    start, first, previous, gaps = time.monotonic(), None, None, []
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
            if not any(c.get("text") for c in event.get("choices", [])):
                continue
            now = time.monotonic()
            if first is None:
                first = now - start
            if previous is not None:
                gaps.append(now - previous)
            previous = now
    if first is None:
        raise RuntimeError("stream returned no content")
    return first, gaps


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
    args = p.parse_args()
    if min(args.requests, args.concurrency, args.max_tokens, args.timeout) <= 0:
        p.error("request count, concurrency, max tokens, and timeout must be positive")
    start, results, errors = time.monotonic(), [], []
    with concurrent.futures.ThreadPoolExecutor(max_workers=args.concurrency) as pool:
        futures = [pool.submit(request, args) for _ in range(args.requests)]
        for f in concurrent.futures.as_completed(futures):
            try:
                results.append(f.result())
            except Exception as exc:
                errors.append(str(exc))
    elapsed = time.monotonic() - start
    ttft = [r[0] for r in results]
    gaps = [gap for _, request_gaps in results for gap in request_gaps]
    print(json.dumps({"requests": args.requests, "concurrency": args.concurrency,
                      "max_tokens": args.max_tokens, "completed": len(results),
                      "errors": errors, "elapsed_seconds": elapsed,
                      "requests_per_second": len(results) / elapsed,
                      "ttft_seconds": {str(q): percentile(ttft, q) for q in [.5, .95, .99]},
                      "mean_content_chunk_gap_seconds": statistics.mean(gaps) if gaps else None}, indent=2))
    if errors:
        raise SystemExit(1)


if __name__ == "__main__":
    main()
