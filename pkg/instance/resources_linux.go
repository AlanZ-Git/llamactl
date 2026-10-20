//go:build linux

package instance

import (
	"context"
	"fmt"
	"io"
	"llamactl/pkg/resources"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

type ownedStat struct {
	group           int
	birth, cpu, rss uint64
}

func parseOwnedStat(data []byte) (ownedStat, error) {
	// comm may contain spaces and parentheses; it is never retained or published.
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return ownedStat{}, fmt.Errorf("invalid stat")
	}
	f := strings.Fields(string(data[end+1:]))
	if len(f) < 22 {
		return ownedStat{}, fmt.Errorf("short stat")
	}
	group, err := strconv.Atoi(f[2])
	if err != nil {
		return ownedStat{}, err
	}
	vals := make([]uint64, 4)
	for n, index := range []int{11, 12, 19, 21} {
		v, err := strconv.ParseUint(f[index], 10, 64)
		if err != nil {
			return ownedStat{}, err
		}
		vals[n] = v
	}
	if vals[0] > ^uint64(0)-vals[1] {
		return ownedStat{}, fmt.Errorf("counter overflow")
	}
	return ownedStat{group: group, birth: vals[2], cpu: vals[0] + vals[1], rss: vals[3]}, nil
}

func readOwnedFile(root *os.Root, path string) ([]byte, error) {
	f, err := root.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 64*1024+1))
	if len(b) > 64*1024 {
		return nil, io.ErrShortBuffer
	}
	return b, err
}

func (t *ownedTree) sampleResources(ctx context.Context) resources.ProcessReading {
	r := resources.ProcessReading{At: time.Now(), Source: "linux_proc_owned", Coverage: "unavailable", Reason: "tree_lifecycle_busy"}
	if !t.mu.TryLock() {
		return r
	}
	defer t.mu.Unlock()
	if t.exited {
		r.Reason = "owned_tree_exited"
		return r
	}
	if t.evidenceErr != nil || t.cmd == nil || t.cmd.Process == nil {
		r.Reason = "tree_lifetime_unproven"
		return r
	}
	if t.rootReaped {
		r.Reason = "root_reaped_descendants_not_enumerable"
		return r
	}
	// The sole waiter cannot reap/reuse the root or its PGID while this short
	// observation holds mu. Each proc directory FD anchors an individual lifetime;
	// numeric child IDs are accepted only after membership within that FD is read.
	bounded, cancel := context.WithTimeout(ctx, 25*time.Millisecond)
	defer cancel()
	rootPID := t.cmd.Process.Pid
	queue := []int{rootPID}
	seen := map[int]bool{}
	readings := []resources.ProcessReading{}
	reason := "descendant_discovery_not_atomic"
	for len(queue) > 0 && len(seen) < resources.MaxProcesses && bounded.Err() == nil {
		pid := queue[0]
		queue = queue[1:]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		root, err := os.OpenRoot("/proc/" + strconv.Itoa(pid))
		if err != nil {
			reason = "partial_process_observation"
			continue
		}
		func() {
			defer root.Close()
			data, err := readOwnedFile(root, "stat")
			if err != nil {
				reason = "partial_process_observation"
				return
			}
			stat, err := parseOwnedStat(data)
			if err != nil || stat.group != rootPID {
				reason = "membership_not_confirmed"
				return
			}
			sample := resources.ProcessReading{Identity: fmt.Sprintf("%d:%d", pid, stat.birth), PrivateReason: "linux_private_commit_not_available"}
			// Linux /proc exposes USER_HZ ticks (100 on supported Linux Go targets).
			if stat.cpu <= ^uint64(0)/uint64(10*time.Millisecond) {
				ns := stat.cpu * uint64(10*time.Millisecond)
				sample.CPUTimeNS = &ns
			}
			if stat.rss <= ^uint64(0)/uint64(os.Getpagesize()) {
				sample.WorkingSet = resources.Number(float64(stat.rss * uint64(os.Getpagesize())))
			}
			readings = append(readings, sample)
			task, err := root.Open("task")
			if err != nil {
				reason = "partial_child_discovery"
				return
			}
			entries, err := task.ReadDir(resources.MaxProcesses + 1)
			task.Close()
			if err != nil && err != io.EOF {
				reason = "partial_child_discovery"
				return
			}
			if len(entries) > resources.MaxProcesses {
				entries = entries[:resources.MaxProcesses]
				reason = "thread_limit"
			}
			for _, entry := range entries {
				if bounded.Err() != nil {
					break
				}
				if _, err := strconv.Atoi(entry.Name()); err != nil {
					continue
				}
				children, err := readOwnedFile(root, "task/"+entry.Name()+"/children")
				if err != nil {
					reason = "partial_child_discovery"
					continue
				}
				for _, id := range strings.Fields(string(children)) {
					child, err := strconv.Atoi(id)
					if err != nil || child <= 0 {
						continue
					}
					if len(queue)+len(seen) >= resources.MaxProcesses {
						reason = "process_limit"
						break
					}
					if !seen[child] {
						queue = append(queue, child)
					}
				}
			}
		}()
	}
	if bounded.Err() != nil {
		reason = "process_observation_budget"
	}
	sort.Slice(readings, func(a, b int) bool { return readings[a].Identity < readings[b].Identity })
	return sumOwnedReadings(readings, "linux_proc_owned", "root_and_observed_children", reason)
}
