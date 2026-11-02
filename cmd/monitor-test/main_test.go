package main

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/logmon"
)

func TestStreamStats_ReturnsPromptlyOnCancel(t *testing.T) {
	l := logmon.NewWriter(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		streamStats(ctx, 100*time.Millisecond, l)
		close(done)
	}()

	// Wait briefly to allow streamStats to start monitoring and entering select loop
	time.Sleep(20 * time.Millisecond)
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
