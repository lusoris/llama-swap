package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/process"
)

// TestRouter_ShutdownEndsRunLoop pins the run loop's exit: once Shutdown has
// been answered, run() must return and close runDone.
func TestRouter_ShutdownEndsRunLoop(t *testing.T) {
	a := newFakeProcess("a")
	b := newTestBase(t, map[string]process.Process{"a": a}, &stubPlanner{})

	if err := b.Shutdown(time.Second); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case <-b.runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("run loop still alive after Shutdown")
	}
}

// TestRouter_CancelledQueuedRequestNeverLoads proves ServeHTTP tells the
// scheduler when a granted-nothing caller walks away. Request b is deferred
// because evicting a would kill a busy process; b's client then disconnects.
// When a goes idle the scheduler must not start a load for the departed b.
func TestRouter_CancelledQueuedRequestNeverLoads(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	a.serveBlock = make(chan struct{})
	pb := newFakeProcess("b")
	pb.autoReady = true
	b := newTestBase(t, map[string]process.Process{"a": a, "b": pb}, &stubPlanner{
		evict: map[string][]string{"b": {"a"}},
	})

	aDone := make(chan struct{})
	go func() {
		defer close(aDone)
		b.ServeHTTP(httptest.NewRecorder(), newRequest("a"))
	}()
	waitSignal(t, a.serveStarted, "a serve start")

	ctx, cancel := context.WithCancel(context.Background())
	bDone := make(chan struct{})
	go func() {
		defer close(bDone)
		b.ServeHTTP(httptest.NewRecorder(), newRequestCtx(ctx, "b"))
	}()
	waitProcessed(t, b.testProcessed, 2) // a and b both absorbed; b is deferred

	cancel()
	waitSignal(t, bDone, "cancelled b return")

	close(a.serveBlock)
	waitSignal(t, aDone, "a finish")

	select {
	case <-pb.ensureAsked:
		t.Fatal("scheduler started a load for a request whose client had left")
	case <-time.After(300 * time.Millisecond):
	}
}

// TestRouter_AdmissionErrorStopsRequest pins that a refused admission is
// reported to the caller and the granted handler never runs.
func TestRouter_AdmissionErrorStopsRequest(t *testing.T) {
	a := newFakeProcess("a")
	a.autoReady = true
	b := newTestBase(t, map[string]process.Process{"a": a}, &stubPlanner{})

	w := httptest.NewRecorder()
	b.ServeHTTP(w, newRequest("nope"))
	if w.Code == http.StatusOK {
		t.Fatalf("unknown model returned 200 body=%q", w.Body.String())
	}
	if got := a.serveCalls.Load(); got != 0 {
		t.Errorf("a.serveCalls=%d want 0", got)
	}
}
