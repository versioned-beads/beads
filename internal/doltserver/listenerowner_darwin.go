//go:build darwin

package doltserver

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// listenerOwnership uses macOS's built-in lsof and process table only after a
// SQL greeting was received. A greeting cannot establish ownership: another
// process can take the chosen port before dolt binds it. Failure to inspect
// either table is unknown, never evidence that the child owns the listener.
func listenerOwnership(pid, port int) (owned, known bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "/usr/sbin/lsof", "-nP", "-sTCP:LISTEN", "-iTCP:"+strconv.Itoa(port), "-Fp").Output()
	if err != nil {
		return false, false
	}
	listeners := parseListenerPIDs(string(output))
	if len(listeners) == 0 {
		return false, false
	}
	for _, listener := range listeners {
		if listener == pid {
			return true, true
		}
	}
	output, err = exec.CommandContext(ctx, "/bin/ps", "-axo", "pid=,ppid=").Output()
	if err != nil {
		return false, false
	}
	parents := parseProcessParents(string(output))
	for _, listener := range listeners {
		for current := listener; current > 0; current = parents[current] {
			if current == pid {
				return true, true
			}
			if next := parents[current]; next == 0 || next == current {
				break
			}
		}
	}
	return false, true
}

func parseListenerPIDs(output string) []int {
	var pids []int
	for _, line := range strings.Split(output, "\n") {
		if len(line) < 2 || line[0] != 'p' {
			continue
		}
		if pid, err := strconv.Atoi(line[1:]); err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids
}

func parseProcessParents(output string) map[int]int {
	parents := make(map[int]int)
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		parent, parentErr := strconv.Atoi(fields[1])
		if pidErr == nil && parentErr == nil && pid > 0 && parent >= 0 {
			parents[pid] = parent
		}
	}
	return parents
}
