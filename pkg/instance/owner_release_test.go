//go:build linux || windows

package instance

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"llamactl/pkg/backends"
	"llamactl/pkg/config"
	"llamactl/pkg/resourcepolicy"
)

func TestOwnerMonitorClosesExitedGenerationRegardlessOfStatus(t *testing.T) {
	for _, status := range []Status{Failed, Stopped, ShuttingDown} {
		t.Run(statusToName[status], func(t *testing.T) {
			inst, _, _ := treeFixture(t, "stubborn")
			p := inst.process
			p.mu.RLock()
			tree, monitor := p.tree, p.monitorDone
			pipes := []any{p.stdin, p.stdout, p.stderr}
			p.mu.RUnlock()
			inst.SetStatus(status)
			if err := tree.signal(true); err != nil {
				t.Fatal(err)
			}
			waitTreeGone(t, tree)
			waitTreeMonitor(t, monitor)
			inst.logger.mu.RLock()
			open := inst.logger.logFile != nil
			inst.logger.mu.RUnlock()
			if open {
				t.Error("exited generation retained its log handle after monitor completion")
			}
			for _, pipe := range pipes {
				if _, err := pipe.(*os.File).Stat(); err == nil {
					t.Error("exited generation retained a pipe handle")
				}
			}
		})
	}
}

func TestOwnerStaleMonitorCleanupLeavesNewGenerationIntact(t *testing.T) {
	inst, _, _ := treeFixture(t, "graceful")
	p := inst.process
	p.mu.RLock()
	oldCmd, oldTree, oldFinish := p.cmd, p.tree, p.finishIO
	p.mu.RUnlock()
	if err := inst.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := inst.Start(); err != nil {
		t.Fatal(err)
	}
	p.mu.RLock()
	newTree, newContext, newStdin := p.tree, p.ctx, p.stdin
	p.mu.RUnlock()
	inst.logger.mu.RLock()
	newLog := inst.logger.logFile
	inst.logger.mu.RUnlock()
	staleDone := make(chan struct{})
	go p.monitorGeneration(oldCmd, staleDone, oldTree, oldFinish)
	waitTreeMonitor(t, staleDone)
	if newContext.Err() != nil {
		t.Fatal("stale monitor canceled new generation")
	}
	if _, err := newStdin.(*os.File).Stat(); err != nil {
		t.Fatal("stale monitor closed new stdin", err)
	}
	inst.logger.mu.RLock()
	same := inst.logger.logFile == newLog && newLog != nil
	inst.logger.mu.RUnlock()
	if !same {
		t.Fatal("stale monitor closed new log")
	}
	select {
	case <-newTree.done:
		t.Fatal("stale monitor stopped new tree")
	default:
	}
}

func TestOwnerExternalRuntimeStillStopsOwnedLauncherTree(t *testing.T) {
	inst, g, dir := treeFixture(t, "stubborn")
	inst.process.treeDocker = true
	ticket := releaseTicket(t, g, "immediate")
	state, code := inst.ReleaseOwned(ticket)
	ticket.Finish(state, code)
	if state != "unreleased_unknown" || code != "external_runtime" {
		t.Fatal(state, code)
	}
	for _, role := range []string{"root", "child", "grandchild"} {
		if treeReachable(treeAddress(t, dir, role)) {
			t.Fatal("owned launcher tree was skipped", role)
		}
	}
}

func TestOwnerRejectedTerminationAndRetirement(t *testing.T) {
	inst, g, dir := treeFixture(t, "stubborn")
	inst.SetStatus(Failed)
	lease, err := inst.BeginMaintenance()
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Retire(); err == nil {
		t.Fatal("deleted a live tree based on failed status")
	}
	lease.Close()
	if err := inst.Start(); err == nil {
		t.Fatal("started over a live failed tree")
	}
	inst.process.signalTree = func(*ownedTree, bool) error { return fmt.Errorf("injected sensitive path must not escape") }
	ticket := releaseTicket(t, g, "immediate")
	state, code := inst.ReleaseOwned(ticket)
	ticket.Finish(state, code)
	if state != "failed" || code != "termination_failed" {
		t.Fatal(state, code)
	}
	if !treeReachable(treeAddress(t, dir, "grandchild")) {
		t.Fatal("rejected signal killed child")
	}
	payload, _ := json.Marshal(g.Snapshot())
	if strings.Contains(string(payload), "sensitive") {
		t.Fatal("OS diagnostic leaked")
	}
}

func TestOwnerGracefulTerminationDeadlineNeverEscalates(t *testing.T) {
	inst, g, dir := treeFixture(t, "stubborn")
	ticket := releaseTicket(t, g, "graceful")
	state, code := inst.ReleaseOwned(ticket)
	ticket.Finish(state, code)
	if state != "unreleased_unknown" || code != "release_timeout" {
		t.Fatal(state, code)
	}
	if !treeReachable(treeAddress(t, dir, "root")) || !treeReachable(treeAddress(t, dir, "grandchild")) {
		t.Fatal("graceful termination deadline escalated to force")
	}
}

func TestOwnerDelayedExitCannotReportReleased(t *testing.T) {
	inst, g, dir := treeFixture(t, "stubborn")
	// A successful termination API return is not proof of process exit.
	inst.process.signalTree = func(*ownedTree, bool) error { return nil }
	ticket := releaseTicket(t, g, "immediate")
	state, code := inst.ReleaseOwned(ticket)
	ticket.Finish(state, code)
	if state != "unreleased_unknown" || code != "release_timeout" {
		t.Fatal(state, code)
	}
	if !treeReachable(treeAddress(t, dir, "grandchild")) {
		t.Fatal("fixture exited before evidence check")
	}
}

func TestOwnerStartupAssignmentFailureClosesPipes(t *testing.T) {
	inst, _, _ := treeFixture(t, "graceful")
	if err := inst.Stop(); err != nil {
		t.Fatal(err)
	}
	inst.process.launchTree = func(*exec.Cmd) (*ownedTree, error) { return nil, fmt.Errorf("injected assignment failure") }
	if err := inst.Start(); err == nil {
		t.Fatal("startup failure hidden")
	}
	if inst.IsRunning() {
		t.Fatal("failed assignment published running")
	}
	if _, err := inst.process.stdin.(*os.File).Write([]byte("x")); err == nil {
		t.Fatal("startup stdin leaked")
	}
	if _, err := inst.process.stdout.Read(make([]byte, 1)); err == nil {
		t.Fatal("startup stdout leaked")
	}
	if _, err := inst.process.stderr.Read(make([]byte, 1)); err == nil {
		t.Fatal("startup stderr leaked")
	}
}

// Re-exec only this test, with a hard safety deadline. It creates an actual root,
// child and grandchild with independently observable loopback listeners.
func TestOwnerTreeHelper(t *testing.T) {
	marker := -1
	for n, arg := range os.Args {
		if arg == "--owner-tree-fixture" {
			marker = n
			break
		}
	}
	if marker < 0 {
		return
	}
	args := os.Args[marker+1:]
	if len(args) != 4 {
		os.Exit(90)
	}
	role, behavior, dir, port := args[0], args[1], args[2], args[3]
	if behavior == "stubborn" {
		signal.Ignore(os.Interrupt)
	}
	go func() { time.Sleep(60 * time.Second); os.Exit(91) }()
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		os.Exit(92)
	}
	if err := os.WriteFile(filepath.Join(dir, role+".addr"), []byte(ln.Addr().String()), 0600); err != nil {
		os.Exit(93)
	}
	if role == "root" || role == "child" {
		next := "child"
		if role == "child" {
			next = "grandchild"
		}
		exe, _ := os.Executable()
		cmd := exec.Command(exe, "-test.run=^TestOwnerTreeHelper$", "--", "--owner-tree-fixture", next, behavior, dir, "0")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if cmd.Start() != nil {
			os.Exit(94)
		}
		go cmd.Wait()
	}
	stop := make(chan struct{}, 1)
	if behavior != "stubborn" {
		go func() { _, _ = io.Copy(io.Discard, os.Stdin); stop <- struct{}{} }()
	}
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/accepted-close" {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
			_, _ = io.WriteString(conn, "HTTP/1.1 204 No Content\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
			_ = conn.Close() // Server is the active closer, leaving a TIME_WAIT TCB.
			return
		}
		if r.URL.Path == "/quit" {
			w.WriteHeader(204)
			select {
			case stop <- struct{}{}:
			default:
			}
			return
		}
		if r.URL.Path == "/loading" {
			_ = os.WriteFile(filepath.Join(dir, "loading.addr"), []byte("entered"), 0600)
			<-r.Context().Done()
			return
		}
		if r.URL.Path == "/hold" {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: first\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		w.WriteHeader(204)
	})}
	go server.Serve(ln)
	<-stop
	_ = server.Close()
	os.Exit(0)
}

func treeAddress(t *testing.T, dir, role string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(filepath.Join(dir, role+".addr"))
		if err == nil && len(b) > 0 {
			return string(b)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("synthetic tree did not publish " + role)
	return ""
}
func treeReachable(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}
func waitTreeGone(t *testing.T, tree *ownedTree) {
	t.Helper()
	select {
	case <-tree.done:
	case <-time.After(5 * time.Second):
		t.Fatal("owned tree did not exit")
	}
}

func waitTreeMonitor(t *testing.T, done <-chan struct{}) {
	t.Helper()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("owned generation monitor did not finish cleanup")
	}
}

func treeFixture(t *testing.T, behavior string) (*Instance, *resourcepolicy.Gate, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	g, err := resourcepolicy.Open("")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.AppConfig{ResourcePolicy: g}
	cfg.Instances.InstancesDir = t.TempDir()
	cfg.Instances.LogsDir = t.TempDir()
	cfg.Backends.Custom = map[string]config.BackendSettings{"synthetic": {Command: exe, Args: []string{"-test.run=^TestOwnerTreeHelper$", "--", "--owner-tree-fixture", "root", behavior, dir, "{port}"}}}
	opts := &Options{BackendOptions: backends.Options{BackendType: backends.BackendTypeCustom, CustomServerOptions: &backends.CustomServerOptions{Name: "synthetic", Host: "127.0.0.1", Port: port}}}
	inst, err := NewChecked("owned", cfg, opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := inst.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		p := inst.process
		p.mu.RLock()
		tree, monitor := p.tree, p.monitorDone
		p.mu.RUnlock()
		if tree != nil {
			_ = tree.signal(true)
			waitTreeGone(t, tree)
		}
		waitTreeMonitor(t, monitor)
	})
	for _, role := range []string{"root", "child", "grandchild"} {
		treeAddress(t, dir, role)
	}
	return inst, g, dir
}
func releaseTicket(t *testing.T, g *resourcepolicy.Gate, mode string) *resourcepolicy.ReleaseTicket {
	t.Helper()
	ch := make(chan *resourcepolicy.ReleaseTicket, 2)
	g.SetReleaseRunner(func(ticket *resourcepolicy.ReleaseTicket) { ch <- ticket })
	s := g.Snapshot()
	s, err := g.Update(false, s.BootID, s.Revision)
	if err != nil {
		t.Fatal(err)
	}
	auto := <-ch
	if mode == "graceful" {
		return auto
	}
	_, err = g.RequestRelease(s.BootID, s.Revision, "synthetic-immediate", mode)
	if err != nil {
		t.Fatal(err)
	}
	return <-ch
}

func TestOwnerActualTreeExitAndExternalSentinel(t *testing.T) {
	inst, g, dir := treeFixture(t, "stubborn")
	exe, _ := os.Executable()
	sentinelDir := t.TempDir()
	sentinel := exec.Command(exe, "-test.run=^TestOwnerTreeHelper$", "--", "--owner-tree-fixture", "sentinel", "stubborn", sentinelDir, "0")
	if err := sentinel.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sentinel.Process.Kill(); _ = sentinel.Wait() })
	sentinelAddr := treeAddress(t, sentinelDir, "sentinel")
	// Parent exit alone is not tree release. Adopted descendants remain observable.
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get("http://" + treeAddress(t, dir, "root") + "/quit")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	time.Sleep(50 * time.Millisecond)
	select {
	case <-inst.process.tree.done:
		t.Fatal("parent exit falsely completed tree")
	default:
	}
	if !treeReachable(treeAddress(t, dir, "grandchild")) {
		t.Fatal("grandchild did not outlive root")
	}
	ticket := releaseTicket(t, g, "immediate")
	state, code := inst.ReleaseOwned(ticket)
	ticket.Finish(state, code)
	if state != "released" {
		t.Fatalf("release: %s %s", state, code)
	}
	for _, role := range []string{"root", "child", "grandchild"} {
		if treeReachable(treeAddress(t, dir, role)) {
			t.Error("surviving " + role)
		}
	}
	if !treeReachable(sentinelAddr) {
		t.Fatal("external sentinel was killed")
	}
}

func TestOwnerGracefulTreeExit(t *testing.T) {
	inst, g, dir := treeFixture(t, "graceful")
	ticket := releaseTicket(t, g, "graceful")
	state, code := inst.ReleaseOwned(ticket)
	ticket.Finish(state, code)
	if state != "released" {
		t.Fatal(state, code)
	}
	for _, role := range []string{"root", "child", "grandchild"} {
		if treeReachable(treeAddress(t, dir, role)) {
			t.Error("surviving " + role)
		}
	}
}

func TestOwnerNewGenerationUnaffectedByOldRelease(t *testing.T) {
	inst, g, _ := treeFixture(t, "graceful")
	old := releaseTicket(t, g, "graceful")
	state, code := inst.ReleaseOwned(old)
	if state != "released" {
		t.Fatal(state, code)
	}
	old.Finish(state, code)
	s := g.Snapshot()
	if _, err := g.Update(true, s.BootID, s.Revision); err != nil {
		t.Fatal(err)
	}
	if err := inst.Start(); err != nil {
		t.Fatal(err)
	}
	newTree := inst.process.tree
	state, _ = inst.ReleaseOwned(old)
	if state == "released" {
		t.Fatal("stale ticket accepted")
	}
	select {
	case <-newTree.done:
		t.Fatal("old task killed new tree")
	case <-time.After(50 * time.Millisecond):
	}
}

func acceptedTreeConnection(t *testing.T, addr string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, "GET /accepted-close HTTP/1.1\r\nHost: synthetic\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(conn) // Observe server FIN before client close.
	if err != nil || !strings.HasPrefix(string(body), "HTTP/1.1 204") {
		t.Fatalf("accepted connection: %q %v", body, err)
	}
}

func TestOwnerReleaseAfterAcceptedConnection(t *testing.T) {
	inst, g, dir := treeFixture(t, "stubborn")
	acceptedTreeConnection(t, treeAddress(t, dir, "root"))
	ticket := releaseTicket(t, g, "immediate")
	state, code := inst.ReleaseOwned(ticket)
	ticket.Finish(state, code)
	waitTreeGone(t, inst.process.tree)
	if state != "released" {
		t.Fatalf("owned tree exited after accepted traffic: %s/%s", state, code)
	}
}

func TestOwnerPortEvidenceDoesNotFabricateRelease(t *testing.T) {
	cfg := &config.AppConfig{}
	g, _ := resourcepolicy.Open("")
	cfg.ResourcePolicy = g
	cfg.Instances.InstancesDir = t.TempDir()
	cfg.Instances.LogsDir = t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var opts Options
	data := `{"backend_type":"llama_cpp","backend_options":{"host":"127.0.0.1","port":` + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port) + `}}`
	if err := json.Unmarshal([]byte(data), &opts); err != nil {
		t.Fatal(err)
	}
	inst, err := NewChecked("external", cfg, &opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	ticket := releaseTicket(t, g, "immediate")
	state, code := inst.ReleaseOwned(ticket)
	ticket.Finish(state, code)
	if state != "unreleased_unknown" || code != "port_ownership_unknown" {
		t.Fatal(state, code)
	}
	if !treeReachable(ln.Addr().String()) {
		t.Fatal("external listener changed")
	}
}

func TestOwnerLoadingHealthCancellation(t *testing.T) {
	inst, g, dir := treeFixture(t, "stubborn")
	// A valid backend that never returns healthy, with a one-minute caller budget.
	inst.globalBackendSettings.Custom["synthetic"] = config.BackendSettings{HealthPath: "/loading"}
	done := make(chan error, 1)
	go func() { done <- inst.WaitForHealthy(60) }()
	treeAddress(t, dir, "loading")
	ticket := releaseTicket(t, g, "immediate")
	state, code := inst.ReleaseOwned(ticket)
	ticket.Finish(state, code)
	if state != "released" {
		t.Fatal(state, code)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled health reported success")
		}
	case <-time.After(time.Second):
		t.Fatal("loading health wait not canceled")
	}
	if treeReachable(treeAddress(t, dir, "root")) {
		t.Fatal("root survived")
	}
}
