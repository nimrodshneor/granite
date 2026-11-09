// Package bpf hosts the Linux-only build recipe. The object is loaded from disk,
// so ordinary Go builds and the demo also work without Linux or generated files.
package bpf

//go:generate sh -c "bpftool btf dump file /sys/kernel/btf/vmlinux format c > vmlinux.h"
//go:generate clang -O2 -g -target bpf -I. -c scheduler.bpf.c -o scheduler.o
