package process

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

var ErrStartAborted = fmt.Errorf("aborted")

// healthCheckKey marks requests issued by the health check loop, which polls
// the upstream through the same reverse proxy. Their failures are the expected
// shape of a model still booting, so they must not be logged as proxy errors.
type healthCheckKey struct{}

// newProxyErrorHandler builds the ErrorHandler for a model's reverse proxy.
//
// httputil.ReverseProxy's default handler answers every failure with 502, so a
// client hanging up mid-generation is logged as a Bad Gateway and sends
// operators looking at an inference server that was healthy the whole time.
// Cancellation is classified here, where the error is actually known: the
// request is recorded with the client-closed sentinel rather than blamed on the
// upstream, and at debug level because an impatient caller is normal traffic.
// Real upstream failures keep the 502. See #1029.
func newProxyErrorHandler(id string, proxyLogger *logmon.Monitor) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		// Pick the log level first: a cancelled request is never an upstream
		// fault, whether or not the sentinel ends up applying below.
		switch {
		case errors.Is(err, context.Canceled) || r.Context().Err() != nil:
			proxyLogger.Debugf("<%s> request cancelled: %v", id, err)
		case r.Context().Value(healthCheckKey{}) != nil:
			proxyLogger.Debugf("<%s> health check not ready: %v", id, err)
		default:
			proxyLogger.Warnf("<%s> proxy error: %v", id, err)
		}

		// Only a client that actually hung up gets the recorded-only sentinel.
		// A request cancelled server-side (an operator cancelling it from the
		// UI, or shutdown) still has a client waiting, and must be answered —
		// otherwise net/http finalizes it as an empty 200, telling the caller
		// the request succeeded.
		if swaputil.MarkClientClosed(w, r) || swaputil.ResponseStarted(w) {
			return
		}
		w.WriteHeader(http.StatusBadGateway)
	}
}

// cmdWaitDelay is the upper bound the runtime will wait for child I/O to
// drain after the process exits before force-closing the stdout/stderr
// pipes. Required so that cmd.Wait() returns even when a forked grandchild
// inherits and holds the pipes open (e.g. a shell wrapper that backgrounds
// the real binary). killProcess sends the stop signal directly (not via the
// cmd context), so this delay is measured from process exit rather than from
// the stop request, and stays independent of the caller's graceful timeout.
const cmdWaitDelay = 10 * time.Second

// parentCancelGraceTimeout is the graceful timeout used when the process is
// torn down because parentCtx was cancelled (final router teardown or app
// shutdown). In the normal flow the process has already been stopped via
// Stop() by this point, so killProcess is a no-op kill; the short grace just
// bounds the rare case where a process is still alive when its context is cut.
const parentCancelGraceTimeout = time.Second

// startReq asks the run loop to bring the process up. Run and EnsureReady share
// this one request type — and therefore one code path — so there is only ever a
// single way to start a process. block selects the caller's semantics: Run parks
// its response until the process terminates, EnsureReady is answered as soon as
// the process is ready or the start fails.
type startReq struct {
	timeout time.Duration
	respond chan error
	block   bool
}

type stopReq struct {
	timeout time.Duration
	respond chan error
}

type waitReadyReq struct {
	respond chan error
}

type startResult struct {
	cmd       *exec.Cmd
	cmdDone   chan struct{}
	cancel    context.CancelFunc
	handlerFn http.HandlerFunc
	err       error
}

type ProcessCommand struct {
	id        string
	config    config.ModelConfig
	parentCtx context.Context

	processLogger *logmon.Monitor
	proxyLogger   *logmon.Monitor

	// waitDelay is assigned to cmd.WaitDelay when starting the upstream
	// process. Defaults to cmdWaitDelay; tests override it to keep the
	// pipe-close backstop from dominating their runtime.
	waitDelay time.Duration

	startCh     chan startReq
	stopCh      chan stopReq
	waitReadyCh chan waitReadyReq

	// current Status. Written only by run(); read by State() and Status()
	// via atomic load.
	status atomic.Pointer[Status]

	// stores the active reverse-proxy handler when the process is running.
	// Written only by run(); read by ServeHTTP via atomic load.
	handler atomic.Pointer[http.HandlerFunc]

	// lastUse is the unix-nano timestamp of the most recent activity baseline.
	// It is initialized when the process becomes Ready and updated after ServeHTTP completes.
	lastUse  atomic.Int64
	inflight atomic.Int64 // current in-flight ServeHTTP calls
}

var _ Process = (*ProcessCommand)(nil)

func New(
	parentCtx context.Context,
	id string,
	conf config.ModelConfig,
	processLogger *logmon.Monitor,
	proxyLogger *logmon.Monitor,
) (*ProcessCommand, error) {
	p := &ProcessCommand{
		id:            id,
		config:        conf,
		parentCtx:     parentCtx,
		processLogger: processLogger,
		proxyLogger:   proxyLogger,

		startCh:     make(chan startReq),
		stopCh:      make(chan stopReq),
		waitReadyCh: make(chan waitReadyReq),
		waitDelay:   cmdWaitDelay,
	}
	p.status.Store(&Status{State: StateStopped})

	go p.run()
	return p, nil
}

func (p *ProcessCommand) Logger() *logmon.Monitor { return p.processLogger }

// lifecycle holds the mutable state owned by the run() goroutine: the current
// ProcessState, the running *exec.Cmd, and the list of WaitReady subscribers.
// Only run() and the methods below touch it; ServeHTTP reads p.handler and
// p.status concurrently, which is why those are atomics.
type lifecycle struct {
	p         *ProcessCommand
	state     ProcessState
	cmd       *exec.Cmd
	cmdDone   <-chan struct{}
	cmdCancel context.CancelFunc

	readyWaiters []waitReadyReq
	// runResp parks the in-flight Run caller's response channel. The
	// interface contract is that Run blocks until the process is
	// terminated, so we hold this until Stop, parentCtx, or an
	// upstream exit unblocks it via respondRun.
	runResp chan<- error
}

// run is the single-writer goroutine that owns all mutable lifecycle state
// (current ProcessState, the running *exec.Cmd, the active reverse-proxy
// handler, and the list of WaitReady subscribers). Every public method
// (Run / Stop / State / WaitReady) is a thin client that sends a request on
// one of the channels below and waits for a response — this funnels concurrent
// callers through a single serialization point so the state machine never
// observes a race.
func (p *ProcessCommand) run() {
	// p.status mirrors `state` so State() can observe transitions; setState
	// writes both.
	l := &lifecycle{p: p, state: StateStopped}
	for l.step() {
	}
}

func (l *lifecycle) setState(s ProcessState) {
	old := l.state
	l.state = s
	next := &Status{State: s}
	if s == StateReady {
		if old == StateReady {
			next.ReadySince = l.p.status.Load().ReadySince
		} else {
			next.ReadySince = time.Now()
		}
	}
	l.p.status.Store(next)
	if old != s {
		event.Emit(swaputil.ProcessStateChangeEvent{
			ProcessName: l.p.id,
			OldState:    string(old),
			NewState:    string(s),
		})
	}
}

// notifyWaiters wakes every blocked WaitReady caller with the given result.
// It must be called on every transition into a state that resolves the
// "is it ready yet?" question — ready, failed, aborted, shutdown, stopped,
// or exited. Missing one strands the subscriber forever (issue #946).
func (l *lifecycle) notifyWaiters(err error) {
	for _, w := range l.readyWaiters {
		select {
		case w.respond <- err:
		default:
		}
	}
	l.readyWaiters = nil
}

// respondRun delivers the final Run result, if a Run caller is parked.
func (l *lifecycle) respondRun(err error) {
	if l.runResp != nil {
		l.runResp <- err
		l.runResp = nil
	}
}

// clearCmd forgets the running upstream process.
func (l *lifecycle) clearCmd() {
	l.cmd = nil
	l.cmdDone = nil
	l.cmdCancel = nil
}

// step waits for one request and handles it. It reports false once the
// goroutine must exit (parent context cancelled).
func (l *lifecycle) step() bool {
	p := l.p
	select {
	// Shutdown: parent context cancelled. Tear down any running process,
	// wake any pending WaitReady callers with an error, then exit the
	// goroutine permanently. Subsequent public-method calls will fail
	// because parentCtx.Done() unblocks their send-side selects.
	case <-p.parentCtx.Done():
		l.onShutdown()
		return false

	// Upstream exited on its own (not via Stop). Drop handler state,
	// transition to Stopped, and unblock the parked Run caller.
	// cmdDone is nil while no process is running, so this case is
	// dormant outside of StateReady.
	case <-l.cmdDone:
		l.onUpstreamExit()

	// WaitReady: if we're already in a terminal-for-this-question state,
	// respond immediately; otherwise queue the caller and let a future
	// state transition wake them via notifyWaiters.
	case req := <-p.waitReadyCh:
		l.onWaitReady(req)

	// Start the upstream process (Run or EnsureReady — see startReq).
	case req := <-p.startCh:
		return l.onStart(req)

	// Stop: tear down a running process.
	case stop := <-p.stopCh:
		l.onStop(stop)
	}
	return true
}

func (l *lifecycle) onShutdown() {
	p := l.p
	// Mark shutdown before killProcess so concurrent State() readers
	// stop treating this process as ready while the (possibly slow)
	// teardown is in progress.
	l.setState(StateShutdown)
	if l.cmd != nil {
		p.handler.Store(nil)
		p.killProcess(l.cmd, l.cmdCancel, l.cmdDone, parentCancelGraceTimeout)
		l.clearCmd()
	}
	l.notifyWaiters(fmt.Errorf("[%s] shutdown", p.id))
	l.respondRun(fmt.Errorf("[%s] shutdown", p.id))
}

func (l *lifecycle) onUpstreamExit() {
	p := l.p
	if l.cmdCancel != nil {
		l.cmdCancel()
	}
	l.clearCmd()
	p.handler.Store(nil)
	l.setState(StateStopped)
	p.proxyLogger.Warnf("<%s> upstream process exited unexpectedly", p.id)
	// Safety net: readyWaiters is normally empty here because
	// WaitReady is answered immediately while StateReady. Notifying
	// anyway keeps the invariant that no transition into a settled
	// state can leave a subscriber parked forever.
	l.notifyWaiters(fmt.Errorf("[%s] upstream exited unexpectedly", p.id))
	l.respondRun(fmt.Errorf("[%s] upstream exited unexpectedly", p.id))
}

func (l *lifecycle) onWaitReady(req waitReadyReq) {
	switch l.state {
	case StateReady:
		req.respond <- nil
	case StateShutdown:
		req.respond <- fmt.Errorf("[%s] shutdown", l.p.id)
	default:
		l.readyWaiters = append(l.readyWaiters, req)
	}
}

// onStart runs a start request. doStart can take a long time (health-check
// polling), so it runs in a separate goroutine and we wait on resultCh. While
// waiting we also listen for an incoming Stop — that's how callers cancel an
// in-flight start. It reports false when the goroutine must exit.
func (l *lifecycle) onStart(req startReq) bool {
	p := l.p
	// EnsureReady answers straight from the current state when the
	// "is it ready?" question is already settled. There is deliberately
	// no StateStopping case: a stop keeps this loop parked inside
	// killProcess and out of the select, so a request can only ever be
	// received once the stop has finished and state is Stopped again.
	// Waiting on the channel IS the synchronisation — the caller never
	// has to guess the state from outside.
	if !req.block {
		switch l.state {
		case StateReady:
			req.respond <- nil
			return true
		case StateShutdown:
			req.respond <- fmt.Errorf("[%s] shutdown", p.id)
			return true
		}
	}
	// Only valid from StateStopped. For Run this is also the "second
	// Run while already running" rejection.
	if l.state != StateStopped {
		req.respond <- fmt.Errorf("[%s] could not be started in %s state", p.id, l.state)
		return true
	}
	l.setState(StateStarting)

	startCtx, cancelStart := context.WithCancel(context.Background())
	resultCh := make(chan startResult, 1)
	go func() {
		resultCh <- p.doStart(startCtx, req.timeout)
	}()

	pendingStop, alive := l.awaitStart(req, cancelStart, resultCh)
	if !alive {
		return false
	}
	// cancelStart is idempotent; calling it again here ensures the
	// context is released even on the success path (govet leak check).
	cancelStart()
	if pendingStop != nil {
		pendingStop.respond <- nil
	}
	return true
}

// awaitStart waits for the in-flight doStart to finish, be stopped, or be
// shut down. pendingStop holds a Stop request that arrived mid-start, so the
// caller can respond to it AFTER we've finished tearing the start down. alive
// is false when the parent context was cancelled.
func (l *lifecycle) awaitStart(req startReq, cancelStart context.CancelFunc, resultCh chan startResult) (pendingStop *stopReq, alive bool) {
	p := l.p
	select {
	// doStart finished on its own — either successfully (latch
	// cmd/handler and move to Ready) or with an error (back to
	// Stopped). Either way wake WaitReady subscribers and reply
	// to the Run caller.
	case res := <-resultCh:
		l.onStartResult(req, res)

	// Stop arrived while doStart was still running. Cancel the
	// start context to abort it, then wait for doStart to return.
	// If doStart had already crossed the finish line before
	// cancellation took effect, it returns a live cmd that we
	// must kill ourselves. The Run caller gets ErrAbort; the Stop
	// caller is parked in pendingStop and answered by the caller.
	case stop := <-p.stopCh:
		cancelStart()
		res := <-resultCh
		if res.cmd != nil {
			p.killProcess(res.cmd, res.cancel, res.cmdDone, stop.timeout)
		}
		l.setState(StateStopped)
		l.notifyWaiters(ErrStartAborted)
		req.respond <- ErrStartAborted
		pendingStop = &stop

	// Parent context cancelled (e.g. config reload) while doStart
	// was still running. Stop() returns early when parentCtx is
	// done and never sends on stopCh, so we must handle shutdown
	// here to avoid leaving doStart running indefinitely.
	case <-p.parentCtx.Done():
		cancelStart()
		// Mark shutdown before tearing the process down: killProcess
		// may block (e.g. taskkill on Windows is slow to spawn), and
		// callers observing State() should see StateShutdown promptly
		// rather than a stale StateStarting.
		l.setState(StateShutdown)
		res := <-resultCh
		if res.cmd != nil {
			p.killProcess(res.cmd, res.cancel, res.cmdDone, parentCancelGraceTimeout)
		}
		l.notifyWaiters(fmt.Errorf("[%s] shutdown", p.id))
		l.respondRun(fmt.Errorf("[%s] shutdown", p.id))
		return nil, false
	}
	return pendingStop, true
}

// onStartResult applies a finished doStart to the lifecycle state.
func (l *lifecycle) onStartResult(req startReq, res startResult) {
	p := l.p
	if res.err != nil {
		l.setState(StateStopped)
		l.notifyWaiters(res.err)
		req.respond <- res.err
		return
	}
	l.cmd = res.cmd
	l.cmdDone = res.cmdDone
	l.cmdCancel = res.cancel
	fn := res.handlerFn
	p.handler.Store(&fn)
	// A newly ready process starts a fresh idle window. Without this,
	// lastUse is zero on first start or stale after a restart, so TTL
	// can unload it on the first one-second ticker tick.
	p.lastUse.Store(time.Now().UnixNano())
	l.setState(StateReady)
	l.notifyWaiters(nil)
	if req.block {
		// Park the Run response — Run blocks until the process
		// terminates, so we only fire this when Stop, parentCtx,
		// or the upstream exit takes the process down.
		l.runResp = req.respond
	} else {
		// EnsureReady's question is answered: it's ready.
		req.respond <- nil
	}

	// Start TTL goroutine if configured — self-terminates
	// when state leaves StateReady.
	if p.config.UnloadAfter > 0 {
		go p.unloadAfterTTL()
	}
}

// unloadAfterTTL stops the model once it has been idle for UnloadAfter
// seconds. It returns when the state leaves StateReady.
func (p *ProcessCommand) unloadAfterTTL() {
	ttlDuration := time.Duration(p.config.UnloadAfter) * time.Second
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if p.State() != StateReady {
			return
		}
		if p.inflight.Load() != 0 {
			continue
		}
		if time.Since(time.Unix(0, p.lastUse.Load())) > ttlDuration {
			p.proxyLogger.Infof("<%s> Unloading model, TTL of %ds reached", p.id, p.config.UnloadAfter)
			p.Stop(time.Duration(p.config.UnloadTimeout) * time.Second)
			return
		}
	}
}

func (l *lifecycle) onStop(stop stopReq) {
	toreDown := l.cmd != nil
	if l.cmd != nil {
		l.setState(StateStopping)
		l.p.killProcess(l.cmd, l.cmdCancel, l.cmdDone, stop.timeout)
		l.clearCmd()
		l.p.handler.Store(nil)
	}
	// Stop is a no-op (and not an error) when already Stopped — this
	// is what makes it idempotent for callers that don't track state.
	l.setState(StateStopped)
	if toreDown {
		// A process we just killed is not going to become ready, so
		// release anyone parked on that question rather than leaving
		// them stranded (issue #946). Guarded on having actually torn
		// something down: an idempotent no-op Stop must not cancel a
		// subscriber waiting on a start that hasn't happened yet, which
		// is the legitimate `go Run(); WaitReady()` ordering.
		l.notifyWaiters(fmt.Errorf("[%s] stopped", l.p.id))
	}
	l.respondRun(nil)
	stop.respond <- nil
}

// newReverseProxy builds the reverse proxy to the configured upstream.
func (p *ProcessCommand) newReverseProxy(proxyURL *url.URL) *httputil.ReverseProxy {
	reverseProxy := httputil.NewSingleHostReverseProxy(proxyURL)
	reverseProxy.Transport = &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   time.Duration(p.config.Timeouts.Connect) * time.Second,
			KeepAlive: time.Duration(p.config.Timeouts.KeepAlive) * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   time.Duration(p.config.Timeouts.TLSHandshake) * time.Second,
		ResponseHeaderTimeout: time.Duration(p.config.Timeouts.ResponseHeader) * time.Second,
		ExpectContinueTimeout: time.Duration(p.config.Timeouts.ExpectContinue) * time.Second,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       time.Duration(p.config.Timeouts.IdleConn) * time.Second,
		DisableKeepAlives:     p.config.DisableKeepAlives,
	}
	reverseProxy.ErrorHandler = newProxyErrorHandler(p.id, p.proxyLogger)
	reverseProxy.ModifyResponse = func(resp *http.Response) error {
		// Upstreams such as llama-server set their own CORS headers, and
		// ReverseProxy adds rather than replaces them, so both llama-swap's
		// and the upstream's would be sent. Keep only ours; see issue #85.
		swaputil.StripUpstreamCORSHeaders(resp.Header)

		if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
			resp.Header.Set("X-Accel-Buffering", "no")
		}
		return nil
	}
	return reverseProxy
}

// newProxyHandler wraps the reverse proxy in the handler served while Ready.
//
// httputil.ReverseProxy panics with http.ErrAbortHandler when the upstream
// disconnects after response headers have been sent. Recover here so the
// streaming termination is treated as a normal client/upstream disconnect.
// see: https://github.com/golang/go/issues/23643
func (p *ProcessCommand) newProxyHandler(reverseProxy *httputil.ReverseProxy) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					p.proxyLogger.Infof("<%s> recovered from upstream disconnection during streaming", p.id)
				} else {
					p.proxyLogger.Warnf("<%s> recovered from panic: %v", p.id, rec)
				}
			}
		}()
		reverseProxy.ServeHTTP(w, r)
	})
}

// newUpstreamCmd builds the upstream *exec.Cmd.
//
// cmdCtx + cmd.Cancel are wired as a safety net: if the context is ever
// cancelled while the process is alive, cmd.Cancel sends SIGTERM / CmdStop
// and the runtime escalates to SIGKILL after cmd.WaitDelay. In the normal
// teardown path killProcess sends the stop signal directly instead, so
// cmd.WaitDelay only acts as the inherited-pipe backstop measured from
// process exit (see killProcess).
func (p *ProcessCommand) newUpstreamCmd(args []string) (*exec.Cmd, context.CancelFunc) {
	cmdCtx, cmdCancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(cmdCtx, args[0], args[1:]...)
	cmd.Stderr = p.processLogger
	cmd.Stdout = p.processLogger
	cmd.Env = append(cmd.Environ(), p.config.Env...)
	cmd.Cancel = func() error { return p.sendStopSignal(cmd) }
	cmd.WaitDelay = p.waitDelay
	setProcAttributes(cmd)
	return cmd, cmdCancel
}

// watchUpstream waits for the upstream process to exit, logs how it ended and
// closes cmdDone.
func (p *ProcessCommand) watchUpstream(cmd *exec.Cmd, cmdDone chan struct{}) {
	waitErr := cmd.Wait()
	switch st := p.State(); {
	case waitErr == nil:
		p.proxyLogger.Debugf("<%s> process exited cleanly", p.id)
	case st == StateStopping || st == StateShutdown:
		// Expected: we force-terminated the process. A forced kill exits
		// the child with a non-zero code (e.g. taskkill /f on Windows
		// yields exit status 1), so this is not an error.
		p.proxyLogger.Debugf("<%s> process stopped by llama-swap: %v", p.id, waitErr)
	default:
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			p.proxyLogger.Debugf("<%s> process exited: code=%d, err=%v", p.id, exitErr.ExitCode(), waitErr)
		} else {
			p.proxyLogger.Debugf("<%s> process exited with error: %v", p.id, waitErr)
		}
	}
	close(cmdDone)
}

// upstreamStart is the upstream process while doStart is bringing it up.
type upstreamStart struct {
	p         *ProcessCommand
	cmd       *exec.Cmd
	cmdDone   chan struct{}
	cmdCancel context.CancelFunc
	handlerFn http.HandlerFunc
}

func (u *upstreamStart) abort(err error) startResult {
	u.p.killProcess(u.cmd, u.cmdCancel, u.cmdDone, 5*time.Second)
	return startResult{err: err}
}

func (u *upstreamStart) prematureExit() startResult {
	u.cmdCancel()
	return startResult{err: fmt.Errorf("upstream command exited prematurely")}
}

func (u *upstreamStart) ready() startResult {
	return startResult{cmd: u.cmd, cmdDone: u.cmdDone, cancel: u.cmdCancel, handlerFn: u.handlerFn}
}

// startUpstream launches the upstream process and starts watching it.
func (p *ProcessCommand) startUpstream(args []string, handlerFn http.HandlerFunc) (*upstreamStart, error) {
	cmd, cmdCancel := p.newUpstreamCmd(args)

	p.proxyLogger.Debugf("<%s> Executing start command: %s, env: %s", p.id, strings.Join(args, " "), strings.Join(p.config.Env, ", "))

	cmdDone := make(chan struct{})
	if err := cmd.Start(); err != nil {
		cmdCancel()
		return nil, fmt.Errorf("failed to start command '%s': %w", strings.Join(args, " "), err)
	}

	go p.watchUpstream(cmd, cmdDone)

	return &upstreamStart{p: p, cmd: cmd, cmdDone: cmdDone, cmdCancel: cmdCancel, handlerFn: handlerFn}, nil
}

// resolveStart validates the config and returns the sanitized start command
// and the upstream proxy URL.
func (p *ProcessCommand) resolveStart() ([]string, *url.URL, error) {
	if p.config.Proxy == "" {
		return nil, nil, fmt.Errorf("upstream proxy missing")
	}

	args, err := p.config.SanitizedCommand()
	if err != nil {
		return nil, nil, fmt.Errorf("unable to get sanitized command: %w", err)
	}

	proxyURL, err := url.Parse(p.config.Proxy)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid proxy URL %q: %w", p.config.Proxy, err)
	}
	return args, proxyURL, nil
}

func (p *ProcessCommand) doStart(startCtx context.Context, healthCheckTimeout time.Duration) startResult {
	args, proxyURL, err := p.resolveStart()
	if err != nil {
		return startResult{err: err}
	}

	reverseProxy := p.newReverseProxy(proxyURL)
	handlerFn := p.newProxyHandler(reverseProxy)

	up, err := p.startUpstream(args, handlerFn)
	if err != nil {
		return startResult{err: err}
	}

	if startCtx.Err() != nil {
		return up.abort(ErrStartAborted)
	}

	checkEndpoint := strings.TrimSpace(p.config.CheckEndpoint)
	if checkEndpoint == "none" {
		return up.ready()
	}

	// Wait 250ms for the command to start up before health checking
	select {
	case <-startCtx.Done():
		return up.abort(ErrStartAborted)
	case <-time.After(250 * time.Millisecond):
	}

	// The deadline is pushed back each time the reported progress value
	// changes, so healthCheckTimeout bounds how long a load may go without
	// progress rather than how long it may take in total. A message alone does
	// not count: it can change while the load is stuck, e.g. a growing ETA.
	// Without a progress value, the deadline is fixed as before.
	hc := &healthPoller{
		p:        p,
		up:       up,
		startCtx: startCtx,
		proxy:    reverseProxy,
		timeout:  healthCheckTimeout,
		deadline: time.Now().Add(healthCheckTimeout),
	}
	var failure *startResult
	for done := false; !done; {
		done, failure = hc.poll()
	}
	if failure != nil {
		return *failure
	}

	return up.ready()
}

// healthPoller polls the upstream health endpoint through the reverse proxy.
type healthPoller struct {
	p        *ProcessCommand
	up       *upstreamStart
	startCtx context.Context
	proxy    *httputil.ReverseProxy
	timeout  time.Duration

	deadline     time.Time
	lastReport   *LoadingProgress
	lastProgress *float64
}

// poll runs one health check iteration. done is true when polling is over:
// failure is nil if the upstream became healthy, else the start result to
// return.
func (h *healthPoller) poll() (done bool, failure *startResult) {
	p, up, startCtx := h.p, h.up, h.startCtx
	fail := func(res startResult) (bool, *startResult) { return true, &res }
	select {
	case <-startCtx.Done():
		return fail(up.abort(ErrStartAborted))
	case <-up.cmdDone:
		return fail(up.prematureExit())
	default:
	}

	if time.Now().After(h.deadline) {
		if h.lastProgress != nil {
			return fail(up.abort(fmt.Errorf("health check timed out after %v without loading progress", h.timeout)))
		}
		return fail(up.abort(fmt.Errorf("health check timed out after %v", h.timeout)))
	}

	// Tagged so the proxy ErrorHandler logs a not-yet-listening upstream
	// at debug rather than as a proxy error once per poll.
	checkCtx := context.WithValue(startCtx, healthCheckKey{}, true)
	req, _ := http.NewRequestWithContext(checkCtx, "GET", p.config.CheckEndpoint, nil)
	rr := httptest.NewRecorder()
	h.proxy.ServeHTTP(rr, req)
	resp := rr.Result()
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		p.proxyLogger.Infof("<%s> Health check passed on %s%s", p.id, p.config.Proxy, p.config.CheckEndpoint)
		return true, nil
	} else if startCtx.Err() != nil {
		return fail(up.abort(ErrStartAborted))
	}

	h.trackProgress(rr.Body.Bytes())

	select {
	case <-startCtx.Done():
		return fail(up.abort(ErrStartAborted))
	case <-up.cmdDone:
		return fail(up.prematureExit())
	case <-time.After(time.Second):
	}
	return false, nil
}

// trackProgress records a loading report from a failed health check body.
func (h *healthPoller) trackProgress(body []byte) {
	lp, ok := parseLoadingProgress(body)
	if !ok {
		return
	}
	if lp.Progress != nil && (h.lastProgress == nil || *lp.Progress != *h.lastProgress) {
		h.lastProgress = lp.Progress
		h.deadline = time.Now().Add(h.timeout)
	}
	if !lp.equal(h.lastReport) {
		h.lastReport = &lp
		h.p.setLoading(lp)
	}
}

// sendStopSignal runs the configured CmdStop (if any) or sends SIGTERM to
// the upstream process. Wired up as cmd.Cancel so it fires whenever the
// cmd's context is cancelled.
func (p *ProcessCommand) sendStopSignal(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		p.processLogger.Debugf("<%s> sendStopSignal() called with nil cmd or process, nothing to stop", p.id)
		return nil
	}
	pid := cmd.Process.Pid
	if p.config.CmdStop != "" {
		p.processLogger.Debugf("<%s> sendStopSignal() using CmdStop %q for pid %d", p.id, p.config.CmdStop, pid)
		stopArgs, err := config.SanitizeCommand(
			strings.ReplaceAll(p.config.CmdStop, "${PID}", fmt.Sprintf("%d", pid)),
		)
		if err == nil {
			p.processLogger.Debugf("<%s> sendStopSignal() running stop command: %s", p.id, strings.Join(stopArgs, " "))
			stopCmd := exec.Command(stopArgs[0], stopArgs[1:]...)
			stopCmd.Stderr = p.processLogger
			stopCmd.Stdout = p.processLogger
			// Bound the pipe copy so a CmdStop that backgrounds a child
			// holding stdout/stderr cannot block Run() indefinitely.
			stopCmd.WaitDelay = p.waitDelay
			stopCmd.Env = cmd.Env
			setProcAttributes(stopCmd)
			runErr := stopCmd.Run()
			// ErrWaitDelay is only returned when the stop command itself
			// succeeded, so it is not a failure to stop the process.
			if errors.Is(runErr, exec.ErrWaitDelay) {
				p.processLogger.Warnf("<%s> sendStopSignal() stop command exited but a child held its output open; output may be truncated", p.id)
				runErr = nil
			}
			if runErr != nil {
				p.processLogger.Errorf("<%s> sendStopSignal() stop command failed: %v", p.id, runErr)
			} else {
				p.processLogger.Debugf("<%s> sendStopSignal() stop command completed for pid %d", p.id, pid)
			}
			return runErr
		}
		// fall through to SIGTERM if sanitize failed
		p.processLogger.Errorf("<%s> sendStopSignal() failed to sanitize CmdStop %q: %v, falling back to terminateProcessTree", p.id, p.config.CmdStop, err)
	}
	// On Unix this SIGTERMs the whole process group so a forked grandchild
	// (e.g. a shell wrapper that backgrounds the real binary) is taken down
	// with the parent rather than orphaned.
	p.processLogger.Debugf("<%s> sendStopSignal() no CmdStop configured, calling terminateProcessTree for pid %d", p.id, pid)
	termErr := terminateProcessTree(cmd)
	if termErr != nil {
		p.processLogger.Errorf("<%s> sendStopSignal() terminateProcessTree failed for pid %d: %v", p.id, pid, termErr)
	}
	return termErr
}

// killProcess terminates the upstream process. The flow:
//
//  1. Send the graceful stop signal (CmdStop / SIGTERM) directly — NOT by
//     cancelling cmdCtx. Cancelling the context would start cmd.WaitDelay
//     immediately, which force-kills the process WaitDelay after the signal
//     and would silently cap gracefulTimeout at WaitDelay whenever
//     gracefulTimeout is the longer of the two.
//  2. We wait up to gracefulTimeout for the process to exit on its own.
//  3. If still alive, we SIGKILL the process group directly (Unix) so any
//     forked descendant is force-terminated alongside the parent.
//  4. We wait on cmdDone. cmd.WaitDelay (set when the cmd was built) is the
//     critical backstop here: once the process exits, if a forked grandchild
//     inherited the stdout/stderr pipes and is still holding them, the runtime
//     force-closes the pipes WaitDelay after the exit and cmd.Wait() unblocks.
//     Because we never cancelled the context, that WaitDelay timer measures
//     from process exit (see os/exec awaitGoroutines), not from this call.
//     Without WaitDelay this select would hang forever (the v219 bug).
//
// cancel() is still invoked (deferred) to release the context, but only after
// the process has exited and os/exec's ctx watcher has already torn down, so it
// never re-fires cmd.Cancel.
func (p *ProcessCommand) killProcess(cmd *exec.Cmd, cancel context.CancelFunc, cmdDone <-chan struct{}, gracefulTimeout time.Duration) {
	if cancel == nil {
		return
	}
	defer cancel()

	// Deliver CmdStop / SIGTERM in a goroutine so a slow or hanging CmdStop
	// cannot block the run() goroutine; the gracefulTimeout + Process.Kill
	// path below still guarantees teardown.
	if cmd != nil {
		go func() {
			p.proxyLogger.Debugf("[%s] sending stop signal with timeout %v", p.id, gracefulTimeout)
			if err := p.sendStopSignal(cmd); err != nil {
				p.proxyLogger.Warnf("[%s] stop signal failed: %v", p.id, err)
			}
		}()
	}

	timer := time.NewTimer(gracefulTimeout)
	defer timer.Stop()

	select {
	case <-cmdDone:
		return
	case <-timer.C:
	}

	if cmd != nil {
		// SIGKILL the whole process group on Unix so any descendant that
		// ignored or outlived the graceful signal is force-terminated too.
		_ = killProcessTree(cmd)
	}
	<-cmdDone
}

func (p *ProcessCommand) ID() string {
	return p.id
}

func (p *ProcessCommand) Run(timeout time.Duration) error {
	req := startReq{
		timeout: timeout,
		respond: make(chan error, 1),
		block:   true,
	}
	select {
	case p.startCh <- req:
	case <-p.parentCtx.Done():
		return fmt.Errorf("[%s] shutdown", p.id)
	}
	select {
	case err := <-req.respond:
		return err
	case <-p.parentCtx.Done():
		return fmt.Errorf("[%s] shutdown", p.id)
	}
}

// EnsureReady brings the process to a ready state and blocks until it is
// serving. See the Process interface for the full state-by-state contract.
//
// The send on startCh is the synchronisation point: it can only be received
// when the run loop is back at its select, so a stop that is still in progress
// naturally holds the request until the process is really stopped. Callers must
// not inspect State() first — that read races the run loop and is what caused
// issue #946.
func (p *ProcessCommand) EnsureReady(ctx context.Context, timeout time.Duration) error {
	req := startReq{
		timeout: timeout,
		respond: make(chan error, 1),
	}
	select {
	case p.startCh <- req:
	case <-ctx.Done():
		return ctx.Err()
	case <-p.parentCtx.Done():
		return fmt.Errorf("[%s] shutdown", p.id)
	}
	select {
	case err := <-req.respond:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-p.parentCtx.Done():
		return fmt.Errorf("[%s] shutdown", p.id)
	}
}

func (p *ProcessCommand) WaitReady(ctx context.Context) error {
	req := waitReadyReq{respond: make(chan error, 1)}
	select {
	case p.waitReadyCh <- req:
	case <-ctx.Done():
		return ctx.Err()
	case <-p.parentCtx.Done():
		return fmt.Errorf("[%s] shutdown", p.id)
	}
	select {
	case err := <-req.respond:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *ProcessCommand) Stop(timeout time.Duration) error {
	req := stopReq{
		timeout: timeout,
		respond: make(chan error, 1),
	}
	select {
	case p.stopCh <- req:
	case <-p.parentCtx.Done():
		return fmt.Errorf("[%s] shutdown", p.id)
	}
	return <-req.respond
}

func (p *ProcessCommand) State() ProcessState {
	return p.Status().State
}

func (p *ProcessCommand) Status() Status {
	if st := p.status.Load(); st != nil {
		return *st
	}
	return Status{State: StateStopped}
}

func (p *ProcessCommand) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	fn := p.handler.Load()
	if fn == nil {
		swaputil.SendResponse(w, r, http.StatusServiceUnavailable, fmt.Sprintf("[%s] process is not ready", p.id))
		return
	}
	if p.config.Compat.IgnoreWebsockets && swaputil.IsWebSocketUpgrade(r) {
		(*fn)(w, r)
		return
	}
	p.inflight.Add(1)
	defer func() {
		p.lastUse.Store(time.Now().UnixNano())
		p.inflight.Add(-1)
	}()
	(*fn)(w, r)
}
