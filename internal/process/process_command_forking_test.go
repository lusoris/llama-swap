//go:build !windows

package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
)

// waitForLog polls buf until it contains every string in want or the
// deadline passes. CmdStop runs in its own goroutine and its pipe copy can
// land just after Stop returns.
func waitForLog(buf *syncBuffer, want ...string) bool {
	hasAll := func() bool {
		out := buf.String()
		for _, w := range want {
			if !strings.Contains(out, w) {
				return false
			}
		}
		return true
	}
	deadline := time.Now().Add(testReturnTimeout)
	for !hasAll() && time.Now().Before(deadline) {
		time.Sleep(testLogPollInterval)
	}
	return hasAll()
}

// TestProcessCommand_CmdStopOutputLogged verifies that stdout and stderr from
// CmdStop are written to the process logger, the same as the start command.
// It lives in this file for the !windows build tag since it needs sh.
func TestProcessCommand_CmdStopOutputLogged(t *testing.T) {
	skipIfNoSimpleResponder(t)

	port := getFreePort(t)
	logBuf := &syncBuffer{}
	procLogger := logmon.NewWriter(logBuf)
	proxyLogger := logmon.NewWriter(io.Discard)

	// printf builds the markers at runtime so they never appear verbatim in
	// the command text, which is itself logged in some paths.
	p, err := New(context.Background(), t.Name(), config.ModelConfig{
		Cmd:                fmt.Sprintf("%s -port %d -silent", simpleResponderPath, port),
		CmdStop:            `sh -c 'printf "cmdstop-%s\n" out; printf "cmdstop-%s\n" err >&2; kill -TERM ${PID}'`,
		Proxy:              fmt.Sprintf("http://127.0.0.1:%d", port),
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 10,
	}, procLogger, proxyLogger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_ = runAsync(t, p)
	if err := p.Stop(testStopTimeout); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if !waitForLog(logBuf, "cmdstop-out", "cmdstop-err") {
		t.Errorf("expected process log to contain CmdStop stdout and stderr; got:\n%s", logBuf.String())
	}
}

// TestProcessCommand_CmdStopBackgroundChildNotAnError verifies that a CmdStop
// which exits successfully but leaves a background child holding its output
// open is bounded by waitDelay and not reported as a failed stop.
func TestProcessCommand_CmdStopBackgroundChildNotAnError(t *testing.T) {
	skipIfNoSimpleResponder(t)

	port := getFreePort(t)
	logBuf := &syncBuffer{}
	procLogger := logmon.NewWriter(logBuf)
	proxyLogger := logmon.NewWriter(io.Discard)

	p, err := New(context.Background(), t.Name(), config.ModelConfig{
		Cmd:                fmt.Sprintf("%s -port %d -silent", simpleResponderPath, port),
		CmdStop:            `sh -c 'printf "cmdstop-%s\n" bg; sleep 2 & kill -TERM ${PID}'`,
		Proxy:              fmt.Sprintf("http://127.0.0.1:%d", port),
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 10,
	}, procLogger, proxyLogger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p.waitDelay = 200 * time.Millisecond

	_ = runAsync(t, p)
	if err := p.Stop(testStopTimeout); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if !waitForLog(logBuf, "cmdstop-bg", "child held its output open") {
		t.Fatalf("expected CmdStop output and WaitDelay warning in process log; got:\n%s", logBuf.String())
	}
	if strings.Contains(logBuf.String(), "stop command failed") {
		t.Errorf("CmdStop that exited cleanly was logged as a failure:\n%s", logBuf.String())
	}
}

// TestProcessCommand_StopForkingWrapper is a regression for the bug reported
// against v219 where Stop would hang indefinitely when the upstream command
// is a shell wrapper that forks the real binary (e.g. `#!/bin/bash` then
// `"$@"`). After SIGTERM the wrapper dies but the grandchild inherits the
// stdout/stderr pipes; cmd.Wait() blocks waiting for the pipe-copy goroutine
// to drain EOF, which never happens while the grandchild holds the fds.
//
// The fix is cmd.WaitDelay (combined with exec.CommandContext + cmd.Cancel),
// which causes the runtime to force-close the pipes after the delay so
// cmd.Wait() — and therefore Stop — returns.
func TestProcessCommand_StopForkingWrapper(t *testing.T) {
	skipIfNoSimpleResponder(t)

	port := getFreePort(t)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")

	// Wrapper script: backgrounds the child (which inherits stdout/stderr),
	// records its PID for cleanup, then waits. When SIGTERM hits bash it
	// dies without forwarding the signal; the grandchild keeps running and
	// keeps the inherited pipe fds open. This is the scenario reported in
	// the v219 regression.
	wrapper := filepath.Join(dir, "wrapper.sh")
	script := fmt.Sprintf("#!/bin/bash\n%q -port %d -silent &\necho $! > %q\nwait\n",
		simpleResponderPath, port, pidFile)
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Cleanup(func() { killChildFromPidFile(pidFile) })

	p := newProcessCommand(t, config.ModelConfig{
		Cmd:                wrapper,
		Proxy:              fmt.Sprintf("http://127.0.0.1:%d", port),
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 10,
	})
	// Shrink the pipe-close backstop so the test doesn't sit at the
	// production default (10s). Must be set before Run() so doStart picks
	// it up when building the cmd.
	const testWaitDelay = 250 * time.Millisecond
	p.waitDelay = testWaitDelay

	runErr := runAsync(t, p)

	// Stop must return within a bounded time even though the grandchild
	// is still holding the pipe open. Budget is generous on top of
	// testWaitDelay to absorb scheduling jitter on slow CI runners; the
	// pre-fix behaviour was an unbounded hang, so any reasonable cap
	// distinguishes pass from fail.
	stopReturned := make(chan error, 1)
	stopStart := time.Now()
	go func() { stopReturned <- p.Stop(testStopTimeout) }()

	const stopBudget = testWaitDelay + 2*time.Second
	select {
	case err := <-stopReturned:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
		t.Logf("Stop returned in %v", time.Since(stopStart))
	case <-time.After(stopBudget):
		t.Fatalf("Stop did not return within %v — cmd.Wait() likely hung on inherited pipe", stopBudget)
	}

	if got := p.State(); got != StateStopped {
		t.Errorf("after Stop: expected state %s, got %s", StateStopped, got)
	}

	select {
	case <-runErr:
	case <-time.After(testReturnTimeout):
		t.Errorf("Run did not return after Stop")
	}
}

// TestProcessCommand_StopHonorsGracefulTimeout is a regression for the bug
// where cmd.WaitDelay capped the graceful shutdown window. killProcess used to
// cancel the cmd context to deliver SIGTERM, which starts cmd.WaitDelay
// immediately; a process whose SIGTERM handler needs longer than WaitDelay to
// finish was force-killed early even though Stop was given a much longer
// timeout. The fix sends the signal directly so WaitDelay measures from process
// exit (its inherited-pipe backstop role), leaving the graceful window to the
// caller's Stop timeout.
func TestProcessCommand_StopHonorsGracefulTimeout(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "graceful.done")
	ready := filepath.Join(dir, "trap.ready")

	// On SIGTERM, sleep past the (short) WaitDelay, then write the marker and
	// exit cleanly. If WaitDelay still drove the kill, bash would be SIGKILLed
	// mid-handler and the marker would never be written. The ready file is
	// written only after the trap is installed so the test does not race
	// SIGTERM ahead of it (CheckEndpoint:none marks ready before bash runs).
	script := filepath.Join(dir, "graceful.sh")
	body := fmt.Sprintf(
		"#!/bin/bash\ncleanup() { sleep 0.6; echo done > %q; exit 0; }\ntrap cleanup SIGTERM\necho ready > %q\nwhile true; do sleep 0.1; done\n",
		marker, ready,
	)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	p := newProcessCommand(t, config.ModelConfig{
		Cmd:           script,
		Proxy:         "http://127.0.0.1:1", // unused: health check disabled
		CheckEndpoint: "none",
	})
	// WaitDelay shorter than the handler's 0.6s sleep, and far shorter than the
	// Stop timeout below — this is the window the old code mis-killed in.
	p.waitDelay = 200 * time.Millisecond

	runErr := runAsync(t, p)

	// Wait until the trap is installed before stopping.
	if !waitFor(2*time.Second, func() bool {
		_, err := os.Stat(ready)
		return err == nil
	}) {
		t.Fatalf("script did not install SIGTERM trap in time")
	}

	stopStart := time.Now()
	if err := p.Stop(5 * time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	elapsed := time.Since(stopStart)

	// The handler must have run to completion (marker written) rather than
	// being force-killed at waitDelay.
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("graceful handler did not complete (marker missing): %v", err)
	}
	// And Stop must have waited for the handler (>~0.6s), not returned at the
	// 200ms waitDelay.
	if elapsed < 500*time.Millisecond {
		t.Fatalf("Stop returned in %v — process was killed before its graceful handler finished", elapsed)
	}

	if got := p.State(); got != StateStopped {
		t.Errorf("after Stop: expected state %s, got %s", StateStopped, got)
	}
	select {
	case <-runErr:
	case <-time.After(testReturnTimeout):
		t.Errorf("Run did not return after Stop")
	}
}

// TestProcessCommand_StopReapsForkedGrandchild verifies that stopping a forking
// wrapper takes down the backgrounded grandchild too, rather than leaving it as
// an orphan. The fix is Setpgid (runtime_unix.go): the wrapper leads its own
// process group, so the stop signal is delivered to the whole group via the
// negative PID and reaches the grandchild the wrapper never reaped.
func TestProcessCommand_StopReapsForkedGrandchild(t *testing.T) {
	skipIfNoSimpleResponder(t)

	port := getFreePort(t)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")

	wrapper := filepath.Join(dir, "wrapper.sh")
	script := fmt.Sprintf("#!/bin/bash\n%q -port %d -silent &\necho $! > %q\nwait\n",
		simpleResponderPath, port, pidFile)
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Cleanup(func() { killChildFromPidFile(pidFile) })

	p := newProcessCommand(t, config.ModelConfig{
		Cmd:                wrapper,
		Proxy:              fmt.Sprintf("http://127.0.0.1:%d", port),
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 10,
	})

	runErr := runAsync(t, p)

	// Read the grandchild PID the wrapper recorded.
	var childPID int
	if !waitFor(2*time.Second, func() bool {
		childPID = readPIDFile(pidFile)
		return childPID > 0
	}) {
		t.Fatalf("wrapper did not record grandchild PID")
	}

	if err := p.Stop(testStopTimeout); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// After Stop the grandchild must be gone. Signal 0 probes liveness without
	// actually sending a signal; give it a brief window to exit after the
	// group SIGTERM.
	proc, err := os.FindProcess(childPID)
	if err != nil {
		t.Fatalf("FindProcess: %v", err)
	}
	gone := false
	for i := 0; i < 100; i++ {
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			gone = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !gone {
		t.Errorf("grandchild PID %d still alive after Stop — process group was not reaped", childPID)
	}

	select {
	case <-runErr:
	case <-time.After(testReturnTimeout):
		t.Errorf("Run did not return after Stop")
	}
}

// killChildFromPidFile reads a PID written by the wrapper script and SIGKILLs
// it so leaked orphans don't accumulate between test runs. Best-effort.
func killChildFromPidFile(pidFile string) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = proc.Kill()
}

// TestProcessCommand_CmdStopDescendantReapedOnTimeout verifies that when CmdStop
// backgrounds a long sleep child and itself hangs, it is bounded by UnloadTimeout
// and its descendant is terminated when the stop timeout kills the stop process group.
func TestProcessCommand_CmdStopDescendantReapedOnTimeout(t *testing.T) {
	skipIfNoSimpleResponder(t)

	port := getFreePort(t)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "descendant.pid")
	t.Cleanup(func() { killChildFromPidFile(pidFile) })

	logBuf := &syncBuffer{}
	procLogger := logmon.NewWriter(logBuf)
	proxyLogger := logmon.NewWriter(io.Discard)

	p, err := New(context.Background(), t.Name(), config.ModelConfig{
		Cmd:                fmt.Sprintf("%s -port %d -silent", simpleResponderPath, port),
		CmdStop:            fmt.Sprintf("sh -c 'sleep 30 & echo $! > %s; exec sleep 30'", pidFile),
		UnloadTimeout:      1,
		Proxy:              fmt.Sprintf("http://127.0.0.1:%d", port),
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 10,
	}, procLogger, proxyLogger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_ = runAsync(t, p)

	// Stop must trigger CmdStop which hangs until UnloadTimeout (1s) expires.
	stopErr := p.Stop(testStopTimeout)
	if stopErr != nil {
		t.Logf("Stop returned error as expected for timed out CmdStop: %v", stopErr)
	}

	// Read the descendant PID from the pid file.
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", pidFile, err)
	}
	descendantPID, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || descendantPID <= 0 {
		t.Fatalf("invalid descendant PID %q: %v", string(data), err)
	}

	// Check with syscall.Kill(pid, 0) returning ESRCH, polling up to 2 s.
	deadline := time.Now().Add(2 * time.Second)
	gone := false
	for time.Now().Before(deadline) {
		err := syscall.Kill(descendantPID, 0)
		if errors.Is(err, syscall.ESRCH) {
			gone = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !gone {
		t.Fatalf("descendant PID %d still alive after stop returned — process group was not killed", descendantPID)
	}
}

// waitFor polls cond every 10ms until it returns true or timeout elapses.
func waitFor(timeout time.Duration, cond func() bool) bool {
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return true
		}
	}
	return cond()
}

// readPIDFile returns the positive PID stored in path, or 0 when it is missing or malformed.
func readPIDFile(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}
