package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// sleepLevel represents the vLLM sleep level.
type sleepLevel int

const (
	sleepLevel1 sleepLevel = 1

	// sleepHTTPTimeout bounds POST /sleep requests.
	sleepHTTPTimeout = 5 * time.Minute
	// wakeUpHTTPTimeout bounds POST /wake_up requests.
	wakeUpHTTPTimeout = 5 * time.Minute
	// checkHealthyHTTPTimeout bounds GET health check requests.
	checkHealthyHTTPTimeout = 10 * time.Second
)

// serveConfig holds parsed configuration for the serve subcommand.
type serveConfig struct {
	vllmURL     string
	listenAddr  string
	sleepLevel  int
	healthPath  string
	waitTimeout time.Duration
	journalUnit string
	startArgs   []string
}

// vllmWrapper serves as a cmd/cmdStop wrapper for vLLM with sleep mode.
func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: %s <command> [args]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Commands:\n")
		fmt.Fprintf(os.Stderr, "  serve    Start as a forward proxy (for cmd)\n")
		fmt.Fprintf(os.Stderr, "  sleep    Put vLLM to sleep (for cmdStop)\n")
		os.Exit(1)
	}

	ctx := context.Background()
	switch os.Args[1] {
	case "serve":
		serveCmd(ctx, os.Args[2:])
	case "sleep":
		sleepCmd(ctx, os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", os.Args[1])
		os.Exit(1)
	}
}

// parseServeFlags parses command-line flags for the serve subcommand.
func parseServeFlags(args []string) serveConfig {
	var cfg serveConfig
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	fs.StringVar(&cfg.vllmURL, "vllm-url", "", "Base URL of vLLM server (e.g., http://127.0.0.1:8000)")
	fs.StringVar(&cfg.listenAddr, "listen", "", "Address to listen on (e.g., :$PORT)")
	fs.IntVar(&cfg.sleepLevel, "sleep-level", 1, "Sleep level to use when sleeping (default 1)")
	fs.StringVar(&cfg.healthPath, "health-path", "/health", "Health check path (default /health)")
	fs.DurationVar(&cfg.waitTimeout, "wait-timeout", 120*time.Second, "Timeout waiting for daemon to become healthy")
	fs.StringVar(&cfg.journalUnit, "journal-unit", "", "User systemd unit whose logs should be forwarded to stdout")
	fs.Parse(args)
	cfg.startArgs = fs.Args()

	if cfg.vllmURL == "" {
		log.Fatalf("--vllm-url is required")
	}
	if cfg.listenAddr == "" {
		log.Fatalf("--listen is required")
	}
	if len(cfg.startArgs) == 0 {
		log.Fatalf("a daemon command after -- is required")
	}

	cfg.vllmURL = strings.TrimRight(cfg.vllmURL, "/")
	return cfg
}

// ensureAwake ensures that the vLLM daemon is running and awake.
func ensureAwake(ctx context.Context, cfg serveConfig) {
	if err := checkHealthy(ctx, cfg.vllmURL, cfg.healthPath, checkHealthyHTTPTimeout); err != nil {
		log.Printf("vLLM daemon not reachable (%v), attempting to wake up", err)
		if err := wakeUpVLLM(ctx, cfg.vllmURL); err != nil {
			log.Printf("Wake up failed: %v, attempting to start daemon", err)
			if err := startDaemon(ctx, cfg.startArgs, cfg.vllmURL, cfg.healthPath, cfg.waitTimeout); err != nil {
				log.Fatalf("Failed to start daemon: %v", err)
			}
		} else {
			log.Printf("Wake up sent, waiting for healthy state")
			if err := waitForHealthyWithPath(ctx, cfg.vllmURL, cfg.healthPath, cfg.waitTimeout); err != nil {
				log.Fatalf("vLLM health check failed after wake up: %v", err)
			}
		}
	} else {
		log.Printf("vLLM daemon is reachable at %s%s, attempting to wake up if asleep", cfg.vllmURL, cfg.healthPath)
		if err := wakeUpVLLM(ctx, cfg.vllmURL); err != nil {
			log.Printf("Warning: wake up failed (but continuing): %v", err)
		}
		log.Printf("Waiting for vLLM to be healthy after wake up")
		if err := waitForHealthyWithPath(ctx, cfg.vllmURL, cfg.healthPath, 10*time.Second); err != nil {
			log.Fatalf("vLLM health check failed after wake up: %v", err)
		}
	}
}

// newProxyServer constructs the reverse proxy server for vLLM.
func newProxyServer(cfg serveConfig) *http.Server {
	proxyURL, err := url.Parse(cfg.vllmURL)
	if err != nil {
		log.Fatalf("Invalid vLLM URL %q: %v", cfg.vllmURL, err)
	}
	proxy := httputil.NewSingleHostReverseProxy(proxyURL)

	// Disable buffering for SSE; upstream CORS headers stay, llama-swap strips them (issue #85).
	proxy.ModifyResponse = func(resp *http.Response) error {
		if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
			resp.Header.Set("X-Accel-Buffering", "no")
		}
		return nil
	}

	return &http.Server{
		Addr: cfg.listenAddr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			proxy.ServeHTTP(w, r)
		}),
	}
}

// waitForSignalAndShutdown waits for termination signals and shuts down the server.
func waitForSignalAndShutdown(ctx context.Context, srv *http.Server, cfg serveConfig) {
	go func() {
		log.Printf("Starting vllm-wrapper serve on %s -> %s", cfg.listenAddr, cfg.vllmURL)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("ListenAndServe: %v", err)
		}
	}()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	sig := <-c
	signal.Stop(c)

	if sig == syscall.SIGTERM {
		log.Printf("SIGTERM received, putting vLLM to sleep (level %d)", cfg.sleepLevel)
		if err := sleepVLLM(ctx, cfg.vllmURL, cfg.sleepLevel); err != nil {
			log.Printf("Warning: failed to put vLLM to sleep: %v", err)
		} else {
			log.Printf("Successfully put vLLM to sleep (level %d)", cfg.sleepLevel)
		}
	}

	log.Println("Shutting down vllm-wrapper serve...")
	shutdownCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("Server shutdown failed: %v", err)
	}
	log.Println("Server stopped")
}

// serveCmd implements the serve subcommand.
func serveCmd(ctx context.Context, args []string) {
	cfg := parseServeFlags(args)

	journalCtx, stopJournal := context.WithCancel(ctx)
	defer stopJournal()
	if cfg.journalUnit != "" {
		if err := startJournalForwarder(journalCtx, cfg.journalUnit); err != nil {
			log.Printf("Warning: failed to forward logs from %s: %v", cfg.journalUnit, err)
		}
	}

	ensureAwake(ctx, cfg)
	srv := newProxyServer(cfg)
	waitForSignalAndShutdown(ctx, srv, cfg)
}

// guardStopPID refuses a --stop-pid of init, a negative value (a process group), the wrapper itself or its group; 0 means no stop.
func guardStopPID(pid int) error {
	if pid == 0 {
		return nil
	}
	if pid <= 1 {
		return fmt.Errorf("invalid stop PID %d: refusing init or negative PID (must be > 1)", pid)
	}
	if pid == os.Getpid() {
		return fmt.Errorf("invalid stop PID %d: refusing to signal own PID", pid)
	}
	if pid == syscall.Getpgrp() {
		return fmt.Errorf("invalid stop PID %d: refusing to signal own process group", pid)
	}
	return nil
}

// sleepCmd implements the sleep subcommand.
func sleepCmd(ctx context.Context, args []string) {
	var (
		vllmURL    string
		sleepLevel int
		stopPID    int
	)
	fs := flag.NewFlagSet("sleep", flag.ExitOnError)
	fs.StringVar(&vllmURL, "vllm-url", "", "Base URL of vLLM server (e.g., http://127.0.0.1:8000)")
	fs.IntVar(&sleepLevel, "sleep-level", 1, "Sleep level to use (default 1)")
	fs.IntVar(&stopPID, "stop-pid", 0, "PID of the serve proxy to terminate after vLLM successfully enters sleep mode (must be > 1 and not the wrapper itself)")
	fs.Parse(args)

	if vllmURL == "" {
		log.Fatalf("--vllm-url is required")
	}
	if err := guardStopPID(stopPID); err != nil {
		log.Fatalf("invalid --stop-pid: %v", err)
	}
	vllmURL = strings.TrimRight(vllmURL, "/")

	if err := sleepVLLM(ctx, vllmURL, sleepLevel); err != nil {
		log.Fatalf("Failed to put vLLM to sleep: %v", err)
	}

	log.Printf("Successfully put vLLM to sleep (level %d)", sleepLevel)

	if stopPID != 0 {
		if err := syscall.Kill(stopPID, syscall.SIGTERM); err != nil {
			log.Fatalf("Failed to stop serve proxy process %d: %v", stopPID, err)
		}
		log.Printf("Sent SIGTERM to serve proxy process %d", stopPID)
	}
}

// sleepVLLM sends a POST to /sleep to put the vLLM daemon to sleep.
func sleepVLLM(ctx context.Context, vllmURL string, sleepLevel int) error {
	body := map[string]int{"level": sleepLevel}
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("failed to marshal sleep request: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, sleepHTTPTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, vllmURL+"/sleep", strings.NewReader(string(jsonBody)))
	if err != nil {
		return fmt.Errorf("failed to create sleep request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send sleep request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("vLLM sleep request failed with status %d: %s", resp.StatusCode, resp.Status)
	}

	return nil
}

// wakeUpVLLM sends a POST to /wake_up to wake the vLLM daemon.
func wakeUpVLLM(ctx context.Context, vllmURL string) error {
	reqCtx, cancel := context.WithTimeout(ctx, wakeUpHTTPTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, vllmURL+"/wake_up", strings.NewReader(""))
	if err != nil {
		return fmt.Errorf("failed to create /wake_up request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to POST /wake_up: %w", err)
	}
	defer resp.Body.Close()

	// Some vLLM versions answer 204 No Content.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("/wake_up returned unexpected status %d: %s", resp.StatusCode, resp.Status)
	}
	return nil
}

// waitForHealthyWithPath polls the vLLM daemon's health endpoint at the given path.
func waitForHealthyWithPath(ctx context.Context, vllmURL string, healthPath string, timeout time.Duration) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(waitCtx, http.MethodGet, vllmURL+healthPath, nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			if waitCtx.Err() != nil {
				return waitCtx.Err()
			}
			time.Sleep(1 * time.Second)
			continue
		}
		if resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			return nil
		}
		resp.Body.Close()
		time.Sleep(1 * time.Second)
	}
	return waitCtx.Err()
}

// checkHealthy sends a GET request to the health path and returns nil if the response status is 200 OK.
func checkHealthy(ctx context.Context, vllmURL string, healthPath string, timeout time.Duration) error {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, vllmURL+healthPath, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}
	return nil
}

// startJournalForwarder forwards new log entries from a user systemd unit to stdout.
func startJournalForwarder(ctx context.Context, unit string) error {
	cmd := exec.CommandContext(
		ctx,
		"journalctl",
		"--user-unit="+unit,
		"--follow",
		"--lines=0",
		"--output=cat",
		"--no-pager",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start journalctl for %s: %w", unit, err)
	}

	go func() {
		err := cmd.Wait()
		if ctx.Err() == nil && err != nil {
			log.Printf("Journal forwarding for %s stopped: %v", unit, err)
		}
	}()

	return nil
}

// startDaemon executes the start command and waits for the vLLM daemon to become healthy.
func startDaemon(ctx context.Context, startArgs []string, vllmURL string, healthPath string, waitTimeout time.Duration) error {
	cmd := exec.CommandContext(ctx, startArgs[0], startArgs[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start daemon command: %w", err)
	}
	log.Printf("Started daemon with PID %d, waiting for healthy state", cmd.Process.Pid)
	err := waitForHealthyWithPath(ctx, vllmURL, healthPath, waitTimeout)
	if err != nil {
		if killErr := cmd.Process.Kill(); killErr != nil {
			log.Printf("failed to kill process %d: %v", cmd.Process.Pid, killErr)
		}
		return fmt.Errorf("daemon did not become healthy: %w", err)
	}
	// The daemon keeps running; the wrapper never kills it on exit.
	return nil
}
