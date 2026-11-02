package main

import (
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
)

func TestParseFlags(t *testing.T) {
	origCommandLine := flag.CommandLine
	origArgs := os.Args
	defer func() {
		flag.CommandLine = origCommandLine
		os.Args = origArgs
	}()

	tests := []struct {
		name    string
		args    []string
		wantOK  bool
		wantCfg benchmarkConfig
	}{
		{
			name:   "missing model",
			args:   []string{"bench", "-baseurl", "http://localhost:8080/v1"},
			wantOK: false,
		},
		{
			name:   "empty baseurl",
			args:   []string{"bench", "-baseurl", "", "-model", "llama3"},
			wantOK: false,
		},
		{
			name:   "zero requests",
			args:   []string{"bench", "-model", "llama3", "-requests", "0"},
			wantOK: false,
		},
		{
			name:   "negative requests",
			args:   []string{"bench", "-model", "llama3", "-requests", "-5"},
			wantOK: false,
		},
		{
			name:   "zero par",
			args:   []string{"bench", "-model", "llama3", "-par", "0"},
			wantOK: false,
		},
		{
			name:   "negative par",
			args:   []string{"bench", "-model", "llama3", "-par", "-2"},
			wantOK: false,
		},
		{
			name:   "valid default baseurl and par",
			args:   []string{"bench", "-model", "llama3"},
			wantOK: true,
			wantCfg: benchmarkConfig{
				baseurl:         "http://localhost:8080/v1",
				modelName:       "llama3",
				totalRequests:   1,
				parallelization: 1,
			},
		},
		{
			name:   "valid custom args",
			args:   []string{"bench", "-baseurl", "http://example.com/v1", "-model", "test-m", "-requests", "10", "-par", "4"},
			wantOK: true,
			wantCfg: benchmarkConfig{
				baseurl:         "http://example.com/v1",
				modelName:       "test-m",
				totalRequests:   10,
				parallelization: 4,
			},
		},
	}

	for _, tt := range tests {
		os.Args = tt.args
		flag.CommandLine = flag.NewFlagSet(tt.args[0], flag.ContinueOnError)
		flag.CommandLine.SetOutput(io.Discard)

		cfg, ok := parseFlags()
		if ok != tt.wantOK {
			t.Errorf("%s: parseFlags() ok = %v, want %v", tt.name, ok, tt.wantOK)
		}
		if ok && cfg != tt.wantCfg {
			t.Errorf("%s: parseFlags() cfg = %+v, want %+v", tt.name, cfg, tt.wantCfg)
		}
	}
}

func TestRunWorker_Status500(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	cfg := benchmarkConfig{
		baseurl:   ts.URL,
		modelName: "test-model",
	}
	client := ts.Client()

	requests := make(chan int, 1)
	requests <- 1
	close(requests)

	statusCounts := make(map[int]int)
	var mu sync.Mutex

	runWorker(1, cfg, client, requests, statusCounts, &mu)

	mu.Lock()
	defer mu.Unlock()
	if statusCounts[500] != 1 {
		t.Fatalf("expected statusCounts[500] == 1, got %v", statusCounts)
	}
	if statusCounts[-1] != 0 {
		t.Fatalf("expected no client-side errors, got statusCounts[-1] = %d", statusCounts[-1])
	}
}

func TestRunWorker_ClientTimeout(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	cfg := benchmarkConfig{
		baseurl:   ts.URL,
		modelName: "test-model",
	}
	client := &http.Client{
		Timeout: 10 * time.Millisecond,
	}

	requests := make(chan int, 1)
	requests <- 1
	close(requests)

	statusCounts := make(map[int]int)
	var mu sync.Mutex

	runWorker(1, cfg, client, requests, statusCounts, &mu)

	mu.Lock()
	defer mu.Unlock()
	if statusCounts[-1] != 1 {
		t.Fatalf("expected client-side error statusCounts[-1] == 1, got %v", statusCounts)
	}
	if statusCounts[200] != 0 {
		t.Fatalf("expected statusCounts[200] == 0, got %v", statusCounts)
	}
}
