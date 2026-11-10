package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	procRoot       = "/proc"
	cgroupRoot     = "/sys/fs/cgroup"
	cgroupV2Prefix = "0::"
	// /proc/PID/stat field 22, relative to field 3 after the command name.
	processStartTimeIndex = 19
)

func processIdentity(pid uint32, cgroup string, enforce bool) (string, error) {
	root := procRoot + "/" + strconv.FormatUint(uint64(pid), 10)
	b, err := os.ReadFile(root + "/stat")
	if err != nil {
		return "", err
	}
	// comm can contain whitespace and parentheses; fields after its last ')' start at field 3.
	end := strings.LastIndexByte(string(b), ')')
	if end < 0 {
		return "", fmt.Errorf("invalid process stat")
	}
	fields := strings.Fields(string(b[end+1:]))
	if len(fields) <= processStartTimeIndex {
		return "", fmt.Errorf("incomplete process stat")
	}
	membership, err := os.ReadFile(root + "/cgroup")
	if err != nil {
		return "", err
	}
	if enforce {
		found := false
		for _, line := range strings.Split(string(membership), "\n") {
			if strings.HasPrefix(line, cgroupV2Prefix+"/") {
				expected := filepath.Join(cgroupRoot, strings.TrimPrefix(line, cgroupV2Prefix))
				if filepath.Clean(cgroup) != expected || expected == cgroupRoot {
					return "", fmt.Errorf("--cgroup must match the target's non-root host cgroup: %s", expected)
				}
				found = true
			}
		}
		if !found {
			return "", fmt.Errorf("cgroup v2 membership required")
		}
	}
	return fields[processStartTimeIndex] + string(membership), nil
}
