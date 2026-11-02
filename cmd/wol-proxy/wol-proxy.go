package main

import (
	"bufio"
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"
)

//go:embed index.html
var loadingPageHTML string

//go:embed confirm.html
var confirmPageHTML string

// wakePath is the endpoint the confirmation page posts to when -require-confirm is set
const wakePath = "/wol-wake"

// wake durations longer than this are discarded, they most likely mean the
// wake failed and the upstream was started some other way much later
const maxTrackedWake = 10 * time.Minute

const (
	wolTimeout = 5 * time.Second
	wolPort    = "9"
)

var (
	flagMac      = flag.String("mac", "", "mac address to send WoL packet to")
	flagUpstream = flag.String("upstream", "", "upstream proxy address to send requests to")
	flagListen   = flag.String("listen", ":8080", "listen address to listen on")
	flagLog      = flag.String("log", "info", "log level (debug, info, warn, error)")
	flagTimeout  = flag.Int("timeout", 60, "seconds requests wait for upstream response before failing")
	flagAPIKey   = flag.String("api-key", "", "API key sent as Bearer token to the upstream SSE endpoint (falls back to LLAMA_SWAP_API_KEY env var)")

	flagRequireConfirm = flag.Bool("require-confirm", false, "do not wake the upstream automatically; the root page asks for confirmation first and other requests return an error")
)

func initLogging(level string) bool {
	switch level {
	case "debug":
		slog.SetLogLoggerLevel(slog.LevelDebug)
	case "info":
		slog.SetLogLoggerLevel(slog.LevelInfo)
	case "warn":
		slog.SetLogLoggerLevel(slog.LevelWarn)
	case "error":
		slog.SetLogLoggerLevel(slog.LevelError)
	default:
		slog.Error("invalid log level", "logLevel", level)
		return false
	}
	return true
}

func parseAndValidateConfig() (*url.URL, string, bool) {
	if *flagListen == "" {
		slog.Error("listen address is required")
		return nil, "", false
	}
	if *flagMac == "" {
		slog.Error("mac address is required")
		return nil, "", false
	}
	if *flagTimeout < 1 {
		slog.Error("timeout must be greater than 0")
		return nil, "", false
	}
	if _, err := net.ParseMAC(*flagMac); err != nil {
		slog.Error("invalid mac address", "error", err)
		return nil, "", false
	}
	if *flagUpstream == "" {
		slog.Error("upstream proxy address is required")
		return nil, "", false
	}
	upstreamURL, err := url.ParseRequestURI(*flagUpstream)
	if err != nil {
		slog.Error("error parsing upstream url", "error", err)
		return nil, "", false
	}
	apiKey := *flagAPIKey
	if apiKey == "" {
		apiKey = os.Getenv("LLAMA_SWAP_API_KEY")
	}
	return upstreamURL, apiKey, true
}

func runServer(ctx context.Context, upstreamURL *url.URL, apiKey string) {
	proxy := newProxy(ctx, upstreamURL, apiKey)
	server := &http.Server{
		Addr:    *flagListen,
		Handler: proxy,
	}

	go func() {
		slog.Info("server starting on", "address", *flagListen)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("error starting server", "error", err)
		}
	}()

	<-ctx.Done()
	if err := server.Close(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("error closing server", "error", err)
	}
}

func main() {
	flag.Parse()

	if !initLogging(*flagLog) {
		return
	}

	upstreamURL, apiKey, ok := parseAndValidateConfig()
	if !ok {
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	runServer(ctx, upstreamURL, apiKey)
}

type upstreamStatus string

const (
	notready              upstreamStatus = "not ready"
	ready                 upstreamStatus = "ready"
	initialReconnectDelay                = 100 * time.Millisecond
	maxReconnectDelay                    = 10 * time.Second
	healthCheckInterval                  = 10 * time.Second
	healthCheckTimeout                   = 5 * time.Second
)

type proxyServer struct {
	upstreamProxy *httputil.ReverseProxy
	failCount     int
	statusMutex   sync.RWMutex
	status        upstreamStatus
	sseBodyMu     sync.Mutex
	sseBody       io.Closer

	// guarded by statusMutex
	wakeStart time.Time     // when the pending wake began, zero if none
	lastWake  time.Duration // how long the last wake took, zero if unknown
}

func newProxyServer(url *url.URL) *proxyServer {
	return &proxyServer{
		upstreamProxy: httputil.NewSingleHostReverseProxy(url),
		status:        notready,
		failCount:     0,
	}
}

func isDebugEnabled(ctx context.Context) bool {
	return slog.Default().Enabled(ctx, slog.LevelDebug)
}

func discardAndClose(body io.ReadCloser, desc string) {
	if _, err := io.Copy(io.Discard, body); err != nil {
		slog.Debug("failed to discard "+desc, "error", err)
	}
	if err := body.Close(); err != nil {
		slog.Debug("failed to close "+desc, "error", err)
	}
}

func connectSSE(ctx context.Context, client *http.Client, eventsURL string, apiKey string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, eventsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create SSE request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Connection", "keep-alive")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connect to SSE endpoint: %w", err)
	}
	return resp, nil
}

func readSSEEvents(ctx context.Context, body io.Reader) {
	scanner := bufio.NewScanner(body)
	// use a fairly large buffer to avoid scanner errors when reading large SSE events
	buf := make([]byte, 0, 1024*1024*2)
	scanner.Buffer(buf, 1024*1024*2)
	events := 0
	debug := isDebugEnabled(ctx)
	if debug {
		fmt.Print("Events: ")
	}
	for scanner.Scan() {
		if debug {
			events++
			fmt.Printf("%d, ", events)
		}
	}
	fmt.Println()
	if ctx.Err() != nil {
		return
	}
	if err := scanner.Err(); err != nil {
		slog.Error("error reading from SSE stream", "error", err)
	}
}

func runSSEMonitor(ctx context.Context, proxy *proxyServer, eventsURL string, apiKey string) {
	client := &http.Client{Timeout: 0}
	reconnectDelay := time.Duration(0)

	for ctx.Err() == nil {
		if reconnectDelay > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(reconnectDelay):
			}
		}

		slog.Debug("connecting to SSE endpoint", "url", eventsURL)
		resp, err := connectSSE(ctx, client, eventsURL, apiKey)
		if err != nil {
			slog.Warn("failed to connect to SSE endpoint", "error", err)
			proxy.setStatus(notready)
			proxy.incFail(1)
			reconnectDelay = nextReconnectDelay(reconnectDelay)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			slog.Warn("SSE endpoint returned non-OK status", "status", resp.StatusCode)
			discardAndClose(resp.Body, "non-OK SSE body")
			proxy.setStatus(notready)
			proxy.incFail(1)
			reconnectDelay = nextReconnectDelay(reconnectDelay)
			continue
		}

		slog.Info("connected to SSE endpoint, upstream ready")
		proxy.setStatus(ready)
		proxy.resetFailures()
		reconnectDelay = 0
		proxy.setSSEBody(resp.Body)

		readSSEEvents(ctx, resp.Body)

		proxy.setSSEBody(nil)
		if err := resp.Body.Close(); err != nil {
			slog.Debug("failed to close SSE response body", "error", err)
		}
		if ctx.Err() != nil {
			return
		}
		slog.Info("SSE connection closed, upstream not ready")
		proxy.setStatus(notready)
		proxy.incFail(1)
	}
}

// start a goroutine that periodically checks upstream liveness so a
// silently dead connection (e.g. upstream suspended without a TCP close)
// is detected and the upstream is marked not ready again
func runHealthChecker(ctx context.Context, proxy *proxyServer, healthURL string) {
	client := &http.Client{Timeout: healthCheckTimeout}
	ticker := time.NewTicker(healthCheckInterval)
	defer ticker.Stop()

	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			resp, err := client.Get(healthURL)
			if err == nil {
				discardAndClose(resp.Body, "health check body")
				continue
			}
			slog.Warn("upstream health check failed, marking upstream not ready", "error", err)
			proxy.setStatus(notready)
			proxy.incFail(1)
			if c := proxy.takeSSEBody(); c != nil {
				if err := c.Close(); err != nil {
					slog.Debug("failed to close SSE connection on health check failure", "error", err)
				}
			}
		}
	}
}

// newProxy creates a proxy and starts the goroutines that watch the upstream until ctx ends
func newProxy(ctx context.Context, url *url.URL, apiKey string) *proxyServer {
	proxy := newProxyServer(url)

	eventsURL := url.Scheme + "://" + url.Host + "/api/events"
	healthURL := url.Scheme + "://" + url.Host + "/wol-health"

	go runSSEMonitor(ctx, proxy, eventsURL, apiKey)
	go runHealthChecker(ctx, proxy, healthURL)

	return proxy
}

func nextReconnectDelay(current time.Duration) time.Duration {
	if current == 0 {
		return initialReconnectDelay
	}

	next := current * 2
	if next > maxReconnectDelay {
		return maxReconnectDelay
	}
	return next
}

func (p *proxyServer) handleStatus(w http.ResponseWriter) {
	status := string(p.getStatus())
	failCount := p.getFailures()
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "status: %s\n", status)
	fmt.Fprintf(w, "failures: %d\n", failCount)
}

func (p *proxyServer) waitForReady(ctx context.Context, timeout time.Duration) bool {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for waitCtx.Err() == nil {
		select {
		case <-waitCtx.Done():
			return false
		case <-ticker.C:
			if p.getStatus() == ready {
				return true
			}
		}
	}
	return false
}

func (p *proxyServer) handleNotReady(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path
	if strings.HasPrefix(path, "/api/events") {
		slog.Debug("Skipping wake up", "req", path)
		w.WriteHeader(http.StatusNoContent)
		return false
	}

	if *flagRequireConfirm {
		if path == "/" || strings.HasPrefix(path, "/ui/") {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, confirmPageHTML)
			return false
		}
		slog.Info("upstream not ready, confirmation required", "req", path, "from", r.RemoteAddr)
		http.Error(w, "upstream is not running, open the root page to confirm starting it", http.StatusServiceUnavailable)
		return false
	}

	slog.Info("upstream not ready, sending magic packet", "req", path, "from", r.RemoteAddr)
	p.beginWake()
	if err := sendMagicPacket(*flagMac); err != nil {
		slog.Warn("failed to send magic WoL packet", "error", err)
	}

	if path == "/" || strings.HasPrefix(path, "/ui/") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, loadingPageHTML)
		return false
	}

	if !p.waitForReady(r.Context(), time.Duration(*flagTimeout)*time.Second) {
		slog.Info("timeout waiting for upstream to be ready")
		http.Error(w, "timeout", http.StatusRequestTimeout)
		return false
	}
	return true
}

func (p *proxyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/status" {
		p.handleStatus(w)
		return
	}

	if *flagRequireConfirm && r.URL.Path == wakePath {
		switch r.Method {
		case http.MethodPost:
			p.handleWake(w, r)
			return
		case http.MethodGet:
			p.handleWakeProgress(w, r)
			return
		}
	}

	if p.getStatus() == notready {
		if !p.handleNotReady(w, r) {
			return
		}
	}

	p.upstreamProxy.ServeHTTP(w, r)
}

// handleWake sends the WoL packet after the user confirmed on the confirmation
// page. It redirects to a GET page so the browser never has to re-submit the
// form when the loading page reloads itself.
func (p *proxyServer) handleWake(w http.ResponseWriter, r *http.Request) {
	if p.getStatus() == ready {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	slog.Info("start confirmed, sending magic packet", "from", r.RemoteAddr)
	p.beginWake()
	if err := sendMagicPacket(*flagMac); err != nil {
		slog.Warn("failed to send magic WoL packet", "error", err)
	}

	http.Redirect(w, r, wakePath, http.StatusSeeOther)
}

// handleWakeProgress shows the loading page while a confirmed wake is pending.
// Without a pending wake there is nothing to wait for, so it goes back to the
// root page.
func (p *proxyServer) handleWakeProgress(w http.ResponseWriter, r *http.Request) {
	if p.getStatus() == ready || !p.wakePending() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, p.loadingPage())
}

// loadingPage returns the loading page with the duration of the last wake, so
// the progress bar can estimate how long this one will take
func (p *proxyServer) loadingPage() string {
	last := p.lastWakeDuration()
	if last <= 0 {
		return loadingPageHTML // no estimate yet
	}
	expected := fmt.Sprintf(`data-expected="%.1f"`, last.Seconds())
	return strings.Replace(loadingPageHTML, `data-expected="0"`, expected, 1)
}

// beginWake marks the start of a wake, the first request of a wake wins
func (p *proxyServer) beginWake() {
	p.statusMutex.Lock()
	defer p.statusMutex.Unlock()
	if p.status == ready {
		return // came up in the meantime, nothing to time
	}
	if p.wakeStart.IsZero() || time.Since(p.wakeStart) > maxTrackedWake {
		p.wakeStart = time.Now()
	}
}

func (p *proxyServer) wakePending() bool {
	p.statusMutex.RLock()
	defer p.statusMutex.RUnlock()
	return !p.wakeStart.IsZero()
}

func (p *proxyServer) lastWakeDuration() time.Duration {
	p.statusMutex.RLock()
	defer p.statusMutex.RUnlock()
	return p.lastWake
}

func (p *proxyServer) getStatus() upstreamStatus {
	p.statusMutex.RLock()
	defer p.statusMutex.RUnlock()
	return p.status
}

func (p *proxyServer) setStatus(status upstreamStatus) {
	p.statusMutex.Lock()
	defer p.statusMutex.Unlock()
	p.status = status

	if status == ready && !p.wakeStart.IsZero() {
		if d := time.Since(p.wakeStart); d <= maxTrackedWake {
			p.lastWake = d
			slog.Info("upstream ready after wake", "took", d.Round(100*time.Millisecond))
		}
		p.wakeStart = time.Time{}
	}
}

func (p *proxyServer) incFail(num int) {
	p.statusMutex.Lock()
	defer p.statusMutex.Unlock()
	p.failCount += num
}

func (p *proxyServer) getFailures() int {
	p.statusMutex.RLock()
	defer p.statusMutex.RUnlock()
	return p.failCount
}

func (p *proxyServer) resetFailures() {
	p.statusMutex.Lock()
	defer p.statusMutex.Unlock()
	p.failCount = 0
}

func (p *proxyServer) setSSEBody(c io.Closer) {
	p.sseBodyMu.Lock()
	defer p.sseBodyMu.Unlock()
	p.sseBody = c
}

func (p *proxyServer) takeSSEBody() io.Closer {
	p.sseBodyMu.Lock()
	defer p.sseBodyMu.Unlock()
	c := p.sseBody
	p.sseBody = nil
	return c
}

func buildMagicPacket(macAddr string) ([]byte, error) {
	hwAddr, err := net.ParseMAC(macAddr)
	if err != nil {
		return nil, fmt.Errorf("parse MAC address %q: %w", macAddr, err)
	}
	if len(hwAddr) != 6 {
		return nil, errors.New("invalid MAC address: must be 6 bytes")
	}

	packet := make([]byte, 102)
	for i := 0; i < 6; i++ {
		packet[i] = 0xFF
	}
	for i := 1; i <= 16; i++ {
		copy(packet[i*6:], hwAddr)
	}
	return packet, nil
}

func sendMagicPacket(macAddr string) error {
	packet, err := buildMagicPacket(macAddr)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), wolTimeout)
	defer cancel()

	dialer := net.Dialer{Timeout: wolTimeout}
	addr := net.JoinHostPort(net.IPv4bcast.String(), wolPort)
	conn, err := dialer.DialContext(ctx, "udp", addr)
	if err != nil {
		return fmt.Errorf("dial UDP broadcast %s: %w", addr, err)
	}
	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			slog.Debug("failed to close WoL UDP connection", "error", closeErr)
		}
	}()

	if err := conn.SetWriteDeadline(time.Now().Add(wolTimeout)); err != nil {
		return fmt.Errorf("set UDP write deadline: %w", err)
	}

	if _, err := conn.Write(packet); err != nil {
		return fmt.Errorf("send magic packet: %w", err)
	}
	return nil
}
