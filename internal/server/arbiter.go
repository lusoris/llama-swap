package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/mostlygeek/llama-swap/internal/perf"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/router"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// arbiterAPIVersion is carried in every /api/arbiter response; a change to
// the shape of these responses bumps it.
const arbiterAPIVersion = "arbiter/v1"

// gpuProbe measures the pool from the performance monitor's latest GPU
// snapshot. A device without a total, or sampled longer than maxAge ago, is
// omitted; no snapshot at all is an error.
type gpuProbe struct {
	latest func() []perf.GpuStat
	maxAge time.Duration
	now    func() time.Time
}

func newGpuProbe(mon *perf.Monitor, every time.Duration) gpuProbe {
	if every <= 0 {
		every = 5 * time.Second // performance.every default
	}
	p := gpuProbe{maxAge: 3 * every, now: time.Now}
	if mon != nil {
		p.latest = mon.LatestGpu
	}
	return p
}

func (p gpuProbe) Devices(context.Context) ([]router.Device, error) {
	if p.latest == nil {
		return nil, errors.New("performance monitor disabled")
	}
	stats := p.latest()
	if stats == nil {
		return nil, errors.New("no GPU reading yet")
	}
	now := p.now()
	devs := make([]router.Device, 0, len(stats))
	for _, g := range stats {
		if g.MemTotalMB <= 0 || g.MemUsedMB < 0 || g.MemUsedMB > g.MemTotalMB {
			continue
		}
		if g.Timestamp.IsZero() || now.Sub(g.Timestamp) > p.maxAge {
			continue
		}
		id := g.UUID
		if id == "" {
			id = fmt.Sprintf("gpu%d", g.ID)
		}
		devs = append(devs, router.Device{
			ID:      id,
			Name:    g.Name,
			TotalMB: g.MemTotalMB,
			FreeMB:  g.MemTotalMB - g.MemUsedMB,
			Sampled: g.Timestamp,
		})
	}
	return devs, nil
}

// arbiterLoader is the arbiter's view of the local router. Model names are
// real model IDs; handlers resolve aliases first.
type arbiterLoader struct{ s *Server }

func (l arbiterLoader) ModelState(model string) (process.ProcessState, bool) {
	if !l.s.local.Handles(model) {
		return "", false
	}
	if st, ok := l.s.local.RunningModels()[model]; ok {
		return st, true
	}
	return process.StateStopped, true
}

// Warm loads model the way hooks.onStartup.preload does: a background request
// through the local router, whose scheduler starts the swap.
func (l arbiterLoader) Warm(model string, done func(error)) {
	go func() {
		req, err := http.NewRequestWithContext(l.s.shutdownCtx, http.MethodGet, "/", nil)
		if err != nil {
			done(err)
			return
		}
		req = req.WithContext(swaputil.SetContext(req.Context(), swaputil.ReqContextData{Model: model, ModelID: model, Metadata: make(map[string]string)}))
		dw := &discardResponseWriter{status: http.StatusOK}
		l.s.local.ServeHTTP(dw, req)
		if dw.status >= http.StatusBadRequest {
			done(fmt.Errorf("warming %s: status %d", model, dw.status))
			return
		}
		done(nil)
	}()
}

func writeArbiterJSON(w http.ResponseWriter, status int, body map[string]any) {
	body["api"] = arbiterAPIVersion
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

// handleAPIArbiterPool serves GET /api/arbiter/pool: the measured devices and
// the local models' states. 503 when the pool cannot be measured.
func (s *Server) handleAPIArbiterPool(w http.ResponseWriter, r *http.Request) {
	if s.arbiter == nil {
		writeArbiterJSON(w, http.StatusServiceUnavailable, map[string]any{"error": router.ErrProbe.Error()})
		return
	}
	devs, err := s.arbiter.Pool(r.Context())
	if err != nil {
		writeArbiterJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
		return
	}
	writeArbiterJSON(w, http.StatusOK, map[string]any{
		"devices":   devs,
		"residents": s.local.RunningModels(),
	})
}

// handleAPIArbiterAdmit serves POST /api/arbiter/admit {"model": "..."}:
// 200 admitted, 202 queued with its position, 503 closed, 404 unknown model.
func (s *Server) handleAPIArbiterAdmit(w http.ResponseWriter, r *http.Request) {
	if s.arbiter == nil {
		writeArbiterJSON(w, http.StatusServiceUnavailable, map[string]any{"admission": router.Closed, "error": router.ErrProbe.Error()})
		return
	}
	var body struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil || body.Model == "" {
		writeArbiterJSON(w, http.StatusBadRequest, map[string]any{"error": "body must be {\"model\": \"<name>\"}"})
		return
	}
	model := body.Model
	if real, ok := s.cfg.RealModelName(model); ok {
		model = real
	}

	v, err := s.arbiter.Admit(r.Context(), model, arbiterLoader{s})
	status := http.StatusOK
	switch {
	case errors.Is(err, router.ErrUnknownModel):
		status = http.StatusNotFound
	case v.Admission == router.Closed:
		status = http.StatusServiceUnavailable
	case v.Admission == router.Queued:
		status = http.StatusAccepted
	}
	resp := map[string]any{"model": model, "admission": v.Admission}
	if v.Position > 0 {
		resp["position"] = v.Position
	}
	if err != nil {
		resp["error"] = err.Error()
	}
	writeArbiterJSON(w, status, resp)
}
