package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// requireClosed fails unless ready is already closed.
func requireClosed(t *testing.T, ready <-chan struct{}) {
	t.Helper()
	select {
	case _, ok := <-ready:
		if ok {
			t.Fatal("ready channel delivered a value instead of closing")
		}
	default:
		t.Fatal("ready channel is not closed")
	}
}

func TestStartLoading_ZeroDurationClosesImmediately(t *testing.T) {
	t.Parallel()
	ready := startLoading(0)
	requireClosed(t, ready)
	requireClosed(t, ready)
}

func TestStartLoading_PositiveDurationClosesAfterDelay(t *testing.T) {
	t.Parallel()
	dur := 25 * time.Millisecond
	start := time.Now()
	ready := startLoading(dur)
	select {
	case <-ready:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for ready channel to close")
	}
	if elapsed := time.Since(start); elapsed < dur {
		t.Fatalf("channel closed too quickly: elapsed %v, want at least %v", elapsed, dur)
	}
	requireClosed(t, ready)
}

func TestHealthHandler(t *testing.T) {
	t.Parallel()

	ready := make(chan struct{})
	h := healthHandler(ready)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/health", nil)

	recBefore := httptest.NewRecorder()
	h(recBefore, req)
	if recBefore.Code != http.StatusServiceUnavailable {
		t.Fatalf("before ready close: expected status %d, got %d", http.StatusServiceUnavailable, recBefore.Code)
	}

	close(ready)
	recAfter := httptest.NewRecorder()
	h(recAfter, req)
	if recAfter.Code != http.StatusOK {
		t.Fatalf("after ready close: expected status %d, got %d", http.StatusOK, recAfter.Code)
	}
}

// decodeCompletion checks status and content type, then decodes the body.
func decodeCompletion(t *testing.T, rec *httptest.ResponseRecorder) chatCompletion {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}
	var resp chatCompletion
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response JSON: %v", err)
	}
	return resp
}

func TestHandleNonStreaming_ResponseShape(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	tokens := 5
	handleNonStreaming(rec, context.Background(), tokens, 1000.0)

	resp := decodeCompletion(t, rec)
	got := []string{resp.ID, resp.Object, resp.Model}
	want := []string{"chatcmpl-fake", "chat.completion", "fake-model"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("field %d: got %q, want %q", i, got[i], want[i])
		}
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(resp.Choices))
	}
	if c := resp.Choices[0]; c.FinishReason != "stop" || c.Message.Role != "assistant" {
		t.Errorf("unexpected choice: finish_reason %q, role %q", c.FinishReason, c.Message.Role)
	}
	wantContent := loremText(tokens)
	if resp.Choices[0].Message.Content != wantContent {
		t.Errorf("unexpected message content: got %q, want %q", resp.Choices[0].Message.Content, wantContent)
	}
	if resp.Usage.CompletionTokens != tokens || resp.Usage.TotalTokens != tokens {
		t.Errorf("unexpected usage: %+v, want completion/total tokens %d", resp.Usage, tokens)
	}
}

func TestHandleNonStreaming_CancelledContextWritesNoBody(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	handleNonStreaming(rec, ctx, 100, 1.0)

	if rec.Body.Len() != 0 {
		t.Fatalf("expected empty body on cancelled context, got %d bytes: %q", rec.Body.Len(), rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "" {
		t.Errorf("expected no Content-Type header on cancelled context, got %q", ct)
	}
}

// cancelOnFirstWriteRecorder wraps httptest.ResponseRecorder and cancels a context
// on the first Write call, enabling deterministic mid-stream cancellation tests.
type cancelOnFirstWriteRecorder struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
	writes int
}

func (c *cancelOnFirstWriteRecorder) Write(p []byte) (int, error) {
	c.writes++
	if c.writes == 1 && c.cancel != nil {
		c.cancel()
	}
	return c.ResponseRecorder.Write(p)
}

func (c *cancelOnFirstWriteRecorder) WriteString(s string) (int, error) {
	return c.Write([]byte(s))
}

// hasAssistantContentDelta returns true if at least one chunk has non-empty assistant content.
func hasAssistantContentDelta(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") || strings.HasSuffix(line, "[DONE]") {
			continue
		}
		var chunk chatChunk
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err == nil {
			for _, choice := range chunk.Choices {
				if choice.Delta.Content != "" {
					return true
				}
			}
		}
	}
	return false
}

func TestHandleStreaming(t *testing.T) {
	t.Parallel()

	t.Run("emits chunks and ends with DONE", func(t *testing.T) {
		t.Parallel()
		rec := httptest.NewRecorder()
		tokens := 3
		tps := 500.0

		handleStreaming(rec, context.Background(), tokens, tps)

		if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
			t.Errorf("expected Content-Type text/event-stream, got %q", ct)
		}

		body := rec.Body.String()
		if !strings.HasPrefix(body, "data: ") {
			t.Errorf("expected body to start with 'data: ', got: %q", body)
		}
		if !strings.Contains(body, `"role":"assistant"`) {
			t.Errorf("expected role chunk in stream, got: %q", body)
		}
		if !hasAssistantContentDelta(body) {
			t.Errorf("expected at least one chunk to carry non-empty assistant delta content, got: %q", body)
		}
		if !strings.HasSuffix(body, "data: [DONE]\n\n") {
			t.Errorf("expected stream to end with 'data: [DONE]\\n\\n', got: %q", body)
		}
	})

	t.Run("cancelled mid-stream stops before DONE", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		rec := &cancelOnFirstWriteRecorder{
			ResponseRecorder: httptest.NewRecorder(),
			cancel:           cancel,
		}

		tokens := 10
		tps := 5.0 // 200ms per token, but cancels on first write (role chunk)

		handleStreaming(rec, ctx, tokens, tps)

		body := rec.Body.String()
		if !strings.Contains(body, `"role":"assistant"`) {
			t.Errorf("expected role chunk to be written before cancel, got: %q", body)
		}
		if strings.Contains(body, "[DONE]") {
			t.Fatalf("expected stream to stop before [DONE] on cancellation, got: %q", body)
		}
	})
}
