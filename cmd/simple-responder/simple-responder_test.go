package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func TestSetupRouter_Health(t *testing.T) {
	expectedModel := "TheExpectedModel"
	responseMsg := "hello"
	r := setupRouter(&expectedModel, &responseMsg)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}

	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if resp["status"] != "ok" {
		t.Errorf("expected status 'ok', got %q", resp["status"])
	}
}

func TestSetupRouter_ChatCompletions_NonStreaming(t *testing.T) {
	expectedModel := "TheExpectedModel"
	responseMsg := "custom response"
	r := setupRouter(&expectedModel, &responseMsg)

	payload := `{"messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if resp["responseMessage"] != responseMsg {
		t.Errorf("expected responseMessage %q, got %v", responseMsg, resp["responseMessage"])
	}
	if resp["usage"] == nil {
		t.Errorf("expected usage in response, got %v", resp)
	}
}

func TestSetupRouter_ChatCompletions_Streaming(t *testing.T) {
	expectedModel := "TheExpectedModel"
	responseMsg := "custom response"
	r := setupRouter(&expectedModel, &responseMsg)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions?stream=true", bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "[DONE]") {
		t.Errorf("expected streaming body to contain [DONE], got: %q", body)
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "[DONE]") {
		t.Errorf("expected streaming body to end with [DONE], got: %q", body)
	}

	if !streamHasDeltaContent(body, "asdf") {
		t.Errorf("expected at least one streamed chunk with content %q, got: %q", "asdf", body)
	}
}

// streamHasDeltaContent reports whether an SSE body carries a chunk whose delta content equals want.
func streamHasDeltaContent(body, want string) bool {
	for _, line := range strings.Split(body, "\n") {
		dataStr, ok := strings.CutPrefix(strings.TrimSpace(line), "data:")
		dataStr = strings.TrimSpace(dataStr)
		if !ok || dataStr == "[DONE]" {
			continue
		}
		var parsed struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(dataStr), &parsed) != nil {
			continue
		}
		for _, choice := range parsed.Choices {
			if choice.Delta.Content == want {
				return true
			}
		}
	}
	return false
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) {
	return 0, errors.New("read error")
}

func (errReader) Close() error {
	return nil
}

type errCloseReader struct {
	*strings.Reader
}

func (errCloseReader) Close() error {
	return errors.New("close error")
}

func TestSetupRouter_SDAPI_ImageRoutes(t *testing.T) {
	expectedModel := "TheExpectedModel"
	responseMsg := "hello"
	r := setupRouter(&expectedModel, &responseMsg)

	tests := []struct {
		name  string
		route string
	}{
		{name: "txt2img", route: "/sdapi/v1/txt2img"},
		{name: "img2img", route: "/sdapi/v1/img2img"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := `{"model":"sd-test"}`
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, tt.route, strings.NewReader(payload))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
			}

			var resp struct {
				Model  string   `json:"model"`
				Images []string `json:"images"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshal error: %v", err)
			}
			if resp.Model != "sd-test" {
				t.Errorf("expected model 'sd-test', got %q", resp.Model)
			}
			if resp.Images == nil || len(resp.Images) != 0 {
				t.Errorf("expected empty images array, got %v", resp.Images)
			}
		})
	}
}

func TestSetupRouter_SDAPI_ImageRoutes_ReadError(t *testing.T) {
	expectedModel := "TheExpectedModel"
	responseMsg := "hello"
	r := setupRouter(&expectedModel, &responseMsg)

	tests := []struct {
		name  string
		route string
	}{
		{name: "txt2img", route: "/sdapi/v1/txt2img"},
		{name: "img2img", route: "/sdapi/v1/img2img"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, tt.route, errReader{})
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("expected status %d, got %d", http.StatusInternalServerError, rec.Code)
			}

			var resp map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshal error: %v", err)
			}
			if resp["error"] != "Failed to read request body" {
				t.Errorf("expected error 'Failed to read request body', got %q", resp["error"])
			}
		})
	}
}

func TestSetupRouter_AudioSpeech_CloseError(t *testing.T) {
	expectedModel := "TheExpectedModel"
	responseMsg := "hello"
	r := setupRouter(&expectedModel, &responseMsg)

	var logBuf bytes.Buffer
	origLog := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(origLog)

	payload := `{"model":"TheExpectedModel"}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/audio/speech", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Body = &errCloseReader{Reader: strings.NewReader(payload)}

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}

	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if resp["message"] != "ok" {
		t.Errorf("expected message 'ok', got %q", resp["message"])
	}
	if !strings.Contains(logBuf.String(), "close request body error") {
		t.Errorf("expected close error in log, got: %q", logBuf.String())
	}
}

func TestWaitForShutdown_SigTerm(t *testing.T) {
	origLog := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(origLog)

	sigChan := make(chan os.Signal, 1)
	sigChan <- syscall.SIGTERM

	done := make(chan struct{})
	go func() {
		waitForShutdown(sigChan, false)
		close(done)
	}()

	select {
	case <-done:
		// returned as expected on SIGTERM when ignoreSigTerm=false
	case <-time.After(200 * time.Millisecond):
		t.Fatal("waitForShutdown did not return on SIGTERM with ignoreSigTerm=false")
	}
}

func TestWaitForShutdown_IgnoreSigTerm(t *testing.T) {
	origLog := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(origLog)

	sigChan := make(chan os.Signal, 3)
	done := make(chan struct{})
	go func() {
		waitForShutdown(sigChan, true)
		close(done)
	}()

	// Feed SIGTERM into channel
	sigChan <- syscall.SIGTERM

	select {
	case <-done:
		t.Fatal("waitForShutdown returned on SIGTERM when ignoreSigTerm=true")
	case <-time.After(30 * time.Millisecond):
		// Expected: ignored SIGTERM and still waiting
	}

	// First SIGINT prompts to send another SIGINT (countSigInt becomes 1)
	sigChan <- syscall.SIGINT

	select {
	case <-done:
		t.Fatal("waitForShutdown returned on first SIGINT, expected to require second SIGINT")
	case <-time.After(30 * time.Millisecond):
		// Expected: still waiting
	}

	// Second SIGINT triggers shutdown (countSigInt becomes 2)
	sigChan <- syscall.SIGINT

	select {
	case <-done:
		// Returned as expected
	case <-time.After(200 * time.Millisecond):
		t.Fatal("waitForShutdown did not return after second SIGINT")
	}
}
