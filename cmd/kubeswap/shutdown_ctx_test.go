package main

import (
	"context"
	"net"
	"testing"
	"time"
)

// TestKubeswap_RunUntilShutdownReturnsOnCancel verifies a cancelled caller context ends serving.
func TestKubeswap_RunUntilShutdownReturnsOnCancel(t *testing.T) {
	cfg := testConfig()
	client := newFakeClient()
	if _, err := ensureResources(context.Background(), client, cfg); err != nil {
		t.Fatalf("ensureResources: %v", err)
	}
	s := newServer(cfg, client, "", true, time.Hour, 300*time.Second)
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.runUntilShutdown(ctx, ln) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runUntilShutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runUntilShutdown kept serving after the context was cancelled")
	}
}
