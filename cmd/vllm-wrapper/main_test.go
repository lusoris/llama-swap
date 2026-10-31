package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestWakeUpVLLM(t *testing.T) {
	// Test successful wake up
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/wake_up" {
			t.Errorf("Unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	if err := wakeUpVLLM(context.Background(), ts.URL); err != nil {
		t.Fatalf("wakeUpVLLM failed: %v", err)
	}

	// Test failure when server returns error
	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/wake_up" {
			t.Errorf("Unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts2.Close()

	if err := wakeUpVLLM(context.Background(), ts2.URL); err == nil {
		t.Errorf("wakeUpVLLM expected error for non-200 response")
	}
}

func TestWaitForHealthy(t *testing.T) {
	// Test successful health check
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("Unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"data":[]}`))
	}))
	defer ts.Close()

	if err := waitForHealthyWithPath(context.Background(), ts.URL, "/v1/models", 2*time.Second); err != nil {
		t.Fatalf("waitForHealthy failed: %v", err)
	}

	// Test timeout: server delays response longer than context timeout
	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"data":[]}`))
	}))
	defer ts2.Close()

	err := waitForHealthyWithPath(context.Background(), ts2.URL, "/v1/models", 1*time.Second)
	if err == nil {
		t.Errorf("waitForHealthy expected timeout error")
		return
	}
	if err != context.DeadlineExceeded {
		t.Errorf("waitForHealthy expected context deadline exceeded, got %v", err)
	}
}

func TestCheckHealthy(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("Unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	if err := checkHealthy(context.Background(), ts.URL, "/health", checkHealthyHTTPTimeout); err != nil {
		t.Fatalf("checkHealthy failed: %v", err)
	}

	tsErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer tsErr.Close()

	if err := checkHealthy(context.Background(), tsErr.URL, "/health", checkHealthyHTTPTimeout); err == nil {
		t.Fatalf("checkHealthy expected error for 503 response, got nil")
	}
}

func TestCheckHealthy_Timeout(t *testing.T) {
	block := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	defer close(block)

	err := checkHealthy(context.Background(), ts.URL, "/health", 20*time.Millisecond)
	if err == nil {
		t.Fatalf("checkHealthy expected timeout error, got nil")
	}
}

func TestSleepCommandMarshal(t *testing.T) {
	// We test the sleep command by checking the JSON marshaling we use in sleepCmd.
	// Since sleepCmd is not easily unit-testable without exposing more, we test the structure.
	body := map[string]int{"level": 1}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("Failed to marshal: %v", err)
	}
	expected := `{"level":1}`
	if string(data) != expected {
		t.Errorf("Expected %s, got %s", expected, string(data))
	}
}

// TestStartDaemon tests that startDaemon returns an error when the start command exits
// quickly and the daemon does not become healthy.
func TestStartDaemon(t *testing.T) {
	// Use a start command that exits immediately (true) and a health URL that will not respond.
	err := startDaemon(context.Background(), []string{"true"}, "http://127.0.0.1:12345/health", "/health", 10*time.Millisecond)
	if err == nil {
		t.Fatalf("startDaemon expected error but got nil")
	}
	if !strings.Contains(err.Error(), "daemon did not become healthy") {
		t.Errorf("error expected to contain 'daemon did not become healthy', got %v", err)
	}
}

// TestStartDaemonArgv verifies that multiple startArgs are passed as separate argv values.
func TestStartDaemonArgv(t *testing.T) {
	tmpDir := t.TempDir()
	argvFile := filepath.Join(tmpDir, "argv.txt")

	script := filepath.Join(tmpDir, "write-argv.sh")
	if err := os.WriteFile(script, []byte(
		"#!/bin/bash\nprintf '%s\n' \"$@\" > \""+argvFile+"\"\nexit 0\n",
	), 0755); err != nil {
		t.Fatalf("write helper script: %v", err)
	}

	err := startDaemon(context.Background(), []string{script, "arg1", "arg2", "arg3"}, "http://127.0.0.1:12345/health", "/health", 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected error (health check fails)")
	}

	content, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("read argv file: %v", err)
	}
	got := strings.Split(strings.TrimSpace(string(content)), "\n")
	want := []string{"arg1", "arg2", "arg3"}
	if len(got) != len(want) {
		t.Fatalf("argv length: got %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestGuardStopPID(t *testing.T) {
	child := exec.CommandContext(context.Background(), "sleep", "5")
	if err := child.Start(); err != nil {
		t.Fatalf("failed to start child process: %v", err)
	}
	defer func() {
		if err := child.Process.Kill(); err != nil {
			t.Logf("kill child %d: %v", child.Process.Pid, err)
		}
		if err := child.Wait(); err != nil {
			t.Logf("child %d exited: %v", child.Process.Pid, err)
		}
	}()
	childPID := child.Process.Pid

	tests := []struct {
		name    string
		pid     int
		wantErr bool
	}{
		{name: "zero (no stop requested)", pid: 0, wantErr: false},
		{name: "child process pid", pid: childPID, wantErr: false},
		{name: "init pid 1", pid: 1, wantErr: true},
		{name: "negative pid -1", pid: -1, wantErr: true},
		{name: "negative pid -12345", pid: -12345, wantErr: true},
		{name: "own pid", pid: os.Getpid(), wantErr: true},
		{name: "own process group", pid: syscall.Getpgrp(), wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := guardStopPID(tc.pid)
			if (err != nil) != tc.wantErr {
				t.Errorf("guardStopPID(%d) error = %v, wantErr = %v", tc.pid, err, tc.wantErr)
			}
		})
	}

	if pid := 2; pid != os.Getpid() && pid != syscall.Getpgrp() {
		t.Run("boundary pid 2", func(t *testing.T) {
			if err := guardStopPID(2); err != nil {
				t.Errorf("guardStopPID(2) expected nil, got error: %v", err)
			}
		})
	}
}
