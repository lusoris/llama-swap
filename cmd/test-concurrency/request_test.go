package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// TestSSEData verifies SSE line parsing against a table of expected outputs.
func TestSSEData(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		wantData string
		wantDone bool
		wantOK   bool
	}{
		{
			name:     "non-data line",
			line:     "event: message",
			wantData: "",
			wantDone: false,
			wantOK:   false,
		},
		{
			name:     "empty",
			line:     "data:   ",
			wantData: "",
			wantDone: false,
			wantOK:   false,
		},
		{
			name:     "[DONE]",
			line:     "data: [DONE]",
			wantData: "",
			wantDone: true,
			wantOK:   false,
		},
		{
			name:     "payload",
			line:     `data: {"choices":[{"delta":{"content":"ok"}}]}`,
			wantData: `{"choices":[{"delta":{"content":"ok"}}]}`,
			wantDone: false,
			wantOK:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotData, gotDone, gotOK := sseData(tt.line)
			if gotData != tt.wantData || gotDone != tt.wantDone || gotOK != tt.wantOK {
				t.Errorf("sseData(%q) = (%q, %v, %v), want (%q, %v, %v)",
					tt.line, gotData, gotDone, gotOK, tt.wantData, tt.wantDone, tt.wantOK)
			}
		})
	}
}

// TestEmitChunk_Order verifies reasoning deltas precede content deltas.
func TestEmitChunk_Order(t *testing.T) {
	payload := `{"choices":[{"delta":{"content":"answer","reasoning_content":"thought"}}]}`
	var emitted []string
	sink := func(_ int, text string) {
		emitted = append(emitted, text)
	}

	emitChunk(payload, 0, sink)

	want := []string{"thought", "answer"}
	if !reflect.DeepEqual(emitted, want) {
		t.Errorf("emitChunk order mismatch: got %v, want %v", emitted, want)
	}
}

// TestSendRequest_ContextCancellation verifies that sendRequest respects context cancellation.
func TestSendRequest_ContextCancellation(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := sendRequest(ctx, ts.URL, "test-model", "prompt", 16, 0, func(int, string) {})
	if err == nil {
		t.Fatalf("expected error on cancelled context, got nil")
	}
}
