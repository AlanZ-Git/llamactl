//go:build linux

package instance

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestOwnedResourceLinuxStatAndReapedRoot(t *testing.T) {
	fields := strings.Fields("S 1 123 0 0 0 0 0 0 0 0 20 30 0 0 0 0 0 0 99 4096 2")
	got, err := parseOwnedStat([]byte("123 (private name) with spaces) " + strings.Join(fields, " ")))
	if err != nil || got.group != 123 || got.cpu != 50 || got.birth != 99 || got.rss != 2 {
		t.Fatal(got, err)
	}
	for _, s := range []string{"bad", "123 (x) S 1 2", "123 (x) " + strings.Replace(strings.Join(fields, " "), "20", "-1", 1)} {
		if _, err := parseOwnedStat([]byte(s)); err == nil {
			t.Fatal("invalid stat accepted")
		}
	}
	tree := &ownedTree{rootReaped: true, cmd: &exec.Cmd{Process: &os.Process{Pid: 123}}}
	r := tree.sampleResources(context.Background())
	if r.Reason != "root_reaped_descendants_not_enumerable" || r.WorkingSet != nil {
		t.Fatalf("reaped root PID was reopened: %+v", r)
	}
}
