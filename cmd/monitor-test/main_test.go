package main

import (
	"bufio"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/logmon"
)

func TestStreamStats_ReturnsPromptlyOnCancel(t *testing.T) {
	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create pipe: %v", err)
	}
	os.Stdout = w
	t.Cleanup(func() {
		os.Stdout = origStdout
		closePipe(t, w, r)
	})

	firstLineCh := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(r)
		first := true
		for scanner.Scan() {
			line := scanner.Text()
			if first && (strings.HasPrefix(line, "[SYS") || strings.HasPrefix(line, "[GPU")) {
				first = false
				firstLineCh <- line
			}
		}
	}()

	l := logmon.NewWriter(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		streamStats(ctx, 50*time.Millisecond, l)
		close(done)
	}()

	select {
	case <-firstLineCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first stats line from streamStats within 2s")
	}

	cancel()

	select {
	case <-done:
		// returned promptly on context cancellation
	case <-time.After(2 * time.Second):
		t.Fatal("streamStats did not return within 2s after context was cancelled")
	}
}

func TestStreamStats_AlreadyCancelledContext(t *testing.T) {
	l := logmon.NewWriter(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		streamStats(ctx, 100*time.Millisecond, l)
		close(done)
	}()

	select {
	case <-done:
		// returned promptly
	case <-time.After(2 * time.Second):
		t.Fatal("streamStats did not return within 2s with already cancelled context")
	}
}

// closePipe closes both pipe ends and reports a close error as a test error.
func closePipe(t *testing.T, ends ...io.Closer) {
	t.Helper()
	for _, end := range ends {
		if err := end.Close(); err != nil {
			t.Errorf("close pipe: %v", err)
		}
	}
}
