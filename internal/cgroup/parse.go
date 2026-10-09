// Package cgroup manages the daemon's cgroup v2 subtree and the per-job
// cgroups inside it.
package cgroup

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// cpuPeriod is the cpu.max period in microseconds. It is fixed so that the
// quota is a simple multiple of the requested millicores.
const cpuPeriod = 100000

// CPUMax formats a cpu.max value granting cpuMillis thousandths of a core per
// period ("50000 100000" for 500). Non-positive values mean unlimited.
func CPUMax(cpuMillis int) string {
	if cpuMillis <= 0 {
		return fmt.Sprintf("max %d", cpuPeriod)
	}
	return fmt.Sprintf("%d %d", cpuMillis*(cpuPeriod/1000), cpuPeriod)
}

// ParseKeyValues parses flat-keyed cgroup files such as cpu.stat and
// memory.events, where each line is "key value".
func ParseKeyValues(r io.Reader) (map[string]int64, error) {
	m := make(map[string]int64)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("parse line %q: missing value", line)
		}
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse value of %s: %w", k, err)
		}
		m[k] = n
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read key/value file: %w", err)
	}
	return m, nil
}

// ParseInt parses a single-value cgroup file such as memory.current. The
// literal "max" is reported as -1.
func ParseInt(data []byte) (int64, error) {
	s := string(bytes.TrimSpace(data))
	if s == "max" {
		return -1, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse cgroup value %q: %w", s, err)
	}
	return n, nil
}
