# Granite

A small implementation of the AI Runtime Optimizer proposal: correlate vLLM
latency with Linux runnable delay and test whether a bounded CPU-weight change
helps. Observation is the default. This is an experiment scaffold; no performance
improvement has been established yet.

## Quick start

Requires Go 1.25 or newer. The synthetic demo runs on macOS and Linux without root,
a GPU, Kubernetes, or generated BPF objects:

```sh
make build
./bin/granite demo
make test
```

The demo emits JSON observations and proposed actions: observe, apply, hold,
rollback, and cooldown. Its improvement is synthetic and it never changes the host.

## Components

| Component | Implementation |
| --- | --- |
| `cmd/` | Cobra commands: `agent`, `controller`, `demo` |
| `bpf/` | CO-RE raw scheduler tracepoints, bounded per-thread runnable timestamps, per-CPU totals |
| `pkg/telemetry/` | Cilium program/map loading, attachment cleanup, interval mean scheduling delay |
| `pkg/metrics/` | vLLM histogram deltas and p95 estimation; completed request rate; GPU gauge |
| `pkg/policy/` | Engine-independent evidence gates, trial evaluation and cooldown |
| `pkg/enforce/` | Cgroup v2 CPU-weight boost and restoration |
| `pkg/discovery/` | Read-only Kubernetes Pod inventory using service-account HTTPS |
| `deploy/` | Container/Kubernetes, contention and Prometheus examples |
| `scripts/benchmark.py` | Concurrent streaming request benchmark using Python's standard library |

The node's fast loop needs no Kubernetes API calls. The discovery command is the
controller skeleton: it discovers running labeled Pods, nodes, IPs, UIDs and
container IDs, and emits JSON inventory every 30 seconds. Policies remain local
CLI configuration, so API outages do not interrupt node decisions. Automatic
container-to-host-PID resolution and controller-to-agent policy delivery are
extension points, not implemented features.

## Linux node experiment

Run the current end-to-end agent on a cgroup v2 Linux 5.15+ node with kernel BTF
at `/sys/kernel/btf/vmlinux`, raw scheduler tracepoints, and permission to load
BPF. For the vLLM comparison below, the node also needs GPU utilization from
DCGM. Exclusive GPU access is not a software requirement; an otherwise quiet GPU
just makes the performance comparison easier to interpret. vLLM supports CPU
serving, but Granite's current policy requires a DCGM GPU utilization metric and
cannot evaluate a CPU-only run without a metrics adapter.
macOS can build and run the synthetic demo, but cannot run this Linux eBPF agent.
There is no `make agent` target: `make generate` builds the BPF object, `make build`
builds the CLI, and `./bin/granite agent` starts it.
Install LLVM clang with a BPF backend, bpftool, and libbpf development headers
(for example Ubuntu packages `clang llvm bpftool libbpf-dev`). Apple clang cannot
compile this BPF program. Kernel builds lacking the required BTF fields or hooks
will fail to load rather than silently substitute telemetry. The package command
below is for Ubuntu; use your distribution's equivalents elsewhere. Install Go
1.25 or newer on the Linux node as well.

```sh
test -r /sys/kernel/btf/vmlinux
test "$(stat -fc %T /sys/fs/cgroup)" = cgroup2fs
sudo apt-get update && sudo apt-get install -y clang llvm bpftool libbpf-dev
make generate  # on Linux: generates bpf/vmlinux.h and bpf/scheduler.o
make build
sudo ./bin/granite agent --pid 12345 \
  --object bpf/scheduler.o \
  --app-url http://127.0.0.1:8000/metrics \
  --gpu-url http://127.0.0.1:9400/metrics
```

Replace `12345` with the **host TGID of the engine process**. All its threads are
included; child processes and separate vLLM API/worker processes are not. Choose
the process relevant to the experiment and run one agent per target if necessary,
with distinct listening ports. Restart the agent when the target process restarts;
it checks process start time and cgroup membership to detect PID reuse or movement.

The vLLM endpoint must expose classic Prometheus histograms
`vllm:time_to_first_token_seconds`, `vllm:inter_token_latency_seconds` (or the
older `vllm:time_per_output_token_seconds`), and the `vllm:request_success_total`
counter (also accepts the unsuffixed `vllm:request_success`). These correspond to
the [vLLM production metrics](https://docs.vllm.ai/en/stable/usage/metrics/).
It must represent one workload with a stable
set of labeled series. First scrapes warm up; counter resets, empty intervals,
missing metrics and unsupported histogram ranges suppress trials. Quantiles are
estimated by interpolation over **interval bucket deltas**, rather than lifetime
counters. Metric names may need an adapter for other vLLM versions.

The GPU endpoint must expose exactly one `DCGM_FI_DEV_GPU_UTIL` gauge in percent,
for the GPU serving this workload. A multi-GPU DCGM endpoint must be filtered
before use. Missing or ambiguous GPU data is not evidence of headroom.
Check the actual endpoints before starting the agent:

```sh
curl -fsS http://127.0.0.1:8000/metrics | grep -E '^vllm:(time_to_first_token_seconds|inter_token_latency_seconds|request_success)'
curl -fsS http://127.0.0.1:9400/metrics | grep '^DCGM_FI_DEV_GPU_UTIL{'
```

The first command should show the required classic histogram buckets and completed
request counter. The second should show exactly one GPU utilization sample; filter
the exporter endpoint to the target GPU if it shows more than one. These endpoints
must be reachable from the Linux node where the agent runs.

Inspect JSON logs or the loopback HTTP endpoints:

```sh
curl http://127.0.0.1:8081/status
curl http://127.0.0.1:8081/metrics
curl http://127.0.0.1:8081/healthz
```

`/healthz` returns 503 until application and kernel telemetry are available.
`granite_telemetry_valid` marks unavailable data; other exported values should be
interpreted only when it is 1. The scheduling statistic is an interval **mean
per runnable wait**, not p95 or run-queue depth.

### Enable a trial

Supply the exact non-root host cgroup v2 directory from `/proc/12345/cgroup`.
Record its current weight so you can restore it after a crash:

```sh
cat /proc/12345/cgroup
cat /sys/fs/cgroup/REPLACE_WITH_TARGET_CGROUP/cpu.weight
sudo ./bin/granite agent --pid 12345 \
  --object bpf/scheduler.o \
  --app-url http://127.0.0.1:8000/metrics \
  --gpu-url http://127.0.0.1:9400/metrics \
  --cgroup /sys/fs/cgroup/REPLACE_WITH_TARGET_CGROUP --enforce
```

Default evidence and bounds:

- p95 TTFT above 500ms or inter-token latency above 50ms.
- Mean runnable delay at least 5ms, at least 20 scheduler samples per interval,
  GPU utilization below 85%, for three consecutive 5-second intervals.
- Double the target cgroup's CPU weight, capped at Linux's maximum of 10000.
  A relative weight is not a CPU reservation and does not bypass CPU quota.
- Compare to the pre-trial observation; roll back immediately on >10% latency
  regression, >5% completed-request throughput regression, or unavailable telemetry.
- Restore after 30 seconds even when improved; require a 60-second cooldown.
  Logs distinguish a >=5% improvement from an inconclusive trial.

This limits continuous intervention and supplies repeatable A/B trials. Observation
mode emits the same hypothetical actions with `enforcement_enabled: false`.
The baseline is one observation; statistically robust evaluation belongs in the
benchmark procedure below, not in this small deterministic rule.

Graceful SIGINT/SIGTERM, normal expiry, process identity change, and application
errors attempt restoration. An externally modified weight is left untouched and
reported as a conflict. SIGKILL, node crash, or power loss cannot run restoration;
record the original weight before enabling enforcement and restore it manually
after such a failure. Persistent rollback journaling is future work. A bounded
weight boost alone does not guarantee fairness: measure neighboring workloads.

### Kubernetes scaffold

```sh
docker build -t granite:dev .
kubectl apply -f deploy/controller.yaml
kubectl label pod YOUR_VLLM_POD granite.ai/engine=vllm
kubectl logs deployment/granite-controller
```

Load/push the image into your cluster as needed. The manifests default to the
`default` namespace; adjust the ClusterRoleBinding subject if using another.
The controller only requires `list` permission on Pods. It does not mutate Pods.
Its direct REST client uses in-cluster service-account files; `--api-url`,
`--token-file` and `--ca-file` also support an explicitly configured API endpoint.

For the agent, install the generated BPF object at `/opt/granite/scheduler.o` on
the node, edit the node selector, target TGID, and metrics URLs in
`deploy/agent.yaml`, then apply that file. This template intentionally starts with
an invalid PID until configured. It runs privileged with host PID/network access
for the experiment and mounts cgroups read-only for observation. Enabling writes
requires a writable cgroup mount and explicit `--cgroup` / `--enforce` arguments.
The binary-only container does not build BPF objects at runtime.

### First real comparison

Start one vLLM workload on the Linux GPU node and expose its
[Prometheus metrics](https://docs.vllm.ai/en/stable/usage/metrics/). Run a
[DCGM exporter](https://docs.nvidia.com/datacenter/dcgm/latest/gpu-telemetry/dcgm-exporter.html)
for that GPU. Identify the **host PID of the vLLM engine process**, not just its
API server or container PID, and use that PID in the agent commands above. Keep
model, prompt, token limit, concurrency, and vLLM version fixed in each comparison.
Run `scripts/benchmark.py` from a client that can reach the vLLM API.

1. Start the agent **without** `--enforce`. Run a warmup workload, then a baseline
   benchmark. Confirm `/healthz` returns 200 and `granite_telemetry_valid` is 1
   while requests are flowing. Save the JSON agent output and vLLM/DCGM metrics.
2. Add CPU contention on the same node. If vLLM runs in Kubernetes, set the node
   selector in `deploy/contention.yaml` and apply the five-minute Job below. Run
   the benchmark again while the Job is active. The agent should report runnable
   delay and may propose `apply` with `enforcement_enabled: false` if the evidence
   gates are met.
3. Stop the observation agent, then restart it with the exact vLLM cgroup path and
   `--enforce` as shown above. Repeat the same workload while contention persists.
   Confirm an `apply` decision actually changes `cpu.weight`, followed by a
   `rollback` decision and restoration. Check the saved weight afterward. If the
   Job's five-minute deadline expires first, restart the contention workload
   before measuring the trial.

Example benchmark commands, run from the client (adjust the URL and model):

```sh
python3 scripts/benchmark.py --url http://GPU_NODE:8000 --model YOUR_MODEL --requests 1000 --concurrency 8 > baseline.json
# On the Kubernetes client, after editing the node selector:
kubectl apply -f deploy/contention.yaml
python3 scripts/benchmark.py --url http://GPU_NODE:8000 --model YOUR_MODEL --requests 1000 --concurrency 8 > contention.json
# Restart the agent with --enforce on the Linux node; then run:
python3 scripts/benchmark.py --url http://GPU_NODE:8000 --model YOUR_MODEL --requests 1000 --concurrency 8 > trial.json
kubectl delete job granite-cpu-contention
```

The Job example applies only when vLLM runs in Kubernetes. On another Linux
setup, put a CPU-hungry process in a competing sibling cgroup; a process in the
same cgroup will not test the relative `cpu.weight` change. The target cgroup
must have CPU headroom: `cpu.weight` does not bypass a binding `cpu.max` quota.
Make each run long enough to cover the first scrape, three 5-second evidence
intervals, a 30-second trial, and the 60-second cooldown; increase `--requests`
if needed, and keep the contention active throughout the trial. The default
policy requires p95 TTFT above 500 ms or p95 inter-token latency above 50 ms,
mean runnable delay at least 5 ms with 20 samples per interval, and GPU use below
85%. If any gate is missing, expect `observe` and inspect the agent's `reason`
and `error` fields before changing the workload.

Save agent logs and Prometheus data for all phases. Repeat across prompt
sizes, concurrency, and CPU pressure, randomizing baseline/trial order and allowing
warmup before measuring. Compare TTFT p50/p95/p99, vLLM token latency and throughput,
GPU utilization, scheduling delay, agent overhead and neighboring workload latency.
The Python script measures TTFT and content-chunk gaps; streamed chunks can contain
multiple tokens, so chunk gaps are **not** token ITL. Use engine metrics for ITL.

Run negative controls with a saturated GPU, low CPU delay, or missing GPU telemetry;
there should be no apply decision. No Kubernetes or GPU experiment has been run as
part of this scaffold.

## Next extensions

Automatic Pod/container/cgroup/TGID mapping and policy distribution; per-thread
delay histograms, CPU migrations and execution time; multiple GPU/engine adapters;
durable crash recovery and neighbor fairness budgets; affinity comparison and
statistically stronger trial evaluation. Investigate `sched_ext` only if measured
results show simpler controls are insufficient. GPU kernel scheduling remains
outside this project.
