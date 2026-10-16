package process

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// TestProcess_UpstreamExitMovesToStopped pins the lifecycle transition taken
// when the upstream exits on its own: the state leaves Ready, the handler is
// dropped and the parked Run caller is released with an error.
func TestProcess_UpstreamExitMovesToStopped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses the sleep command")
	}

	p := newProcessCommand(t, config.ModelConfig{
		Cmd:           "sleep 1",
		Proxy:         "http://127.0.0.1:1",
		CheckEndpoint: "none",
	})
	t.Cleanup(func() { p.Stop(testStopTimeout) })

	runErr := runAsync(t, p)
	if got := p.State(); got != StateReady {
		t.Fatalf("after Run: expected state %s, got %s", StateReady, got)
	}

	select {
	case err := <-runErr:
		if err == nil || !strings.Contains(err.Error(), "upstream exited unexpectedly") {
			t.Fatalf("Run() after upstream exit: got %v", err)
		}
	case <-time.After(testStartTimeout):
		t.Fatal("Run() did not return after the upstream exited")
	}

	if got := p.State(); got != StateStopped {
		t.Errorf("after upstream exit: expected state %s, got %s", StateStopped, got)
	}
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, httptest.NewRequest("GET", "/test", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("after upstream exit: expected 503, got %d", rr.Code)
	}
}
