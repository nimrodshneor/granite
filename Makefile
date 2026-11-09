.PHONY: build generate test demo clean
build:
	go build -o bin/granite .

# Run on the Linux node (or a Linux builder with matching kernel BTF).
generate:
	go generate ./bpf

test:
	go test -race ./...

demo:
	go run . demo

clean:
	rm -f bin/granite bpf/scheduler.o bpf/vmlinux.h
