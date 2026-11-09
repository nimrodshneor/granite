//go:build ignore

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>

char LICENSE[] SEC("license") = "Dual BSD/GPL";

struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, 1);
    __type(key, __u32);
    __type(value, __u32);
} target SEC("maps");

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 16384);
    __type(key, __u32);
    __type(value, __u64);
} runnable SEC("maps");

struct counters { __u64 delay_ns; __u64 samples; };
struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, 1);
    __type(key, __u32);
    __type(value, struct counters);
} stats SEC("maps");

static __always_inline int matches(struct task_struct *task) {
    __u32 zero = 0;
    __u32 *tgid = bpf_map_lookup_elem(&target, &zero);
    return tgid && *tgid && BPF_CORE_READ(task, tgid) == *tgid;
}

static __always_inline void enqueue(struct task_struct *task) {
    if (!matches(task)) return;
    __u32 tid = BPF_CORE_READ(task, pid);
    __u64 now = bpf_ktime_get_ns();
    // Repeated wakeups must not shorten an existing runnable interval.
    bpf_map_update_elem(&runnable, &tid, &now, BPF_NOEXIST);
}

SEC("raw_tp/sched_wakeup")
int wakeup(struct bpf_raw_tracepoint_args *ctx) {
    enqueue((struct task_struct *)ctx->args[0]);
    return 0;
}

SEC("raw_tp/sched_wakeup_new")
int wakeup_new(struct bpf_raw_tracepoint_args *ctx) {
    enqueue((struct task_struct *)ctx->args[0]);
    return 0;
}

SEC("raw_tp/sched_switch")
int switch_task(struct bpf_raw_tracepoint_args *ctx) {
    struct task_struct *prev = (void *)ctx->args[1];
    struct task_struct *next = (void *)ctx->args[2];
    // Preemption and voluntary runnable switches both start runnable wait.
    if (ctx->args[0] || BPF_CORE_READ(prev, __state) == 0) enqueue(prev);
    if (!matches(next)) return 0;
    __u32 tid = BPF_CORE_READ(next, pid), zero = 0;
    __u64 *start = bpf_map_lookup_elem(&runnable, &tid);
    if (!start) return 0;
    __u64 delay = bpf_ktime_get_ns() - *start;
    struct counters *s = bpf_map_lookup_elem(&stats, &zero);
    if (s) { s->delay_ns += delay; s->samples++; }
    bpf_map_delete_elem(&runnable, &tid);
    return 0;
}

SEC("raw_tp/sched_process_exit")
int exit_task(struct bpf_raw_tracepoint_args *ctx) {
    __u32 tid = (__u32)bpf_get_current_pid_tgid();
    bpf_map_delete_elem(&runnable, &tid);
    return 0;
}
