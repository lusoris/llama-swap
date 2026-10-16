package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/perf"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/router"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var arbiterNow = time.Date(2026, 10, 16, 20, 0, 0, 0, time.UTC)

func fixedGpuProbe(stats []perf.GpuStat) gpuProbe {
	return gpuProbe{
		latest: func() []perf.GpuStat { return stats },
		maxAge: 15 * time.Second,
		now:    func() time.Time { return arbiterNow },
	}
}

// Only measured, fresh devices join the pool; a device without a total, with
// impossible numbers or with an old or missing sample is left out.
func TestServer_ArbiterProbe_OmitsUnreadableAndStale(t *testing.T) {
	fresh := arbiterNow.Add(-5 * time.Second)
	probe := fixedGpuProbe([]perf.GpuStat{
		{ID: 0, UUID: "GPU-aaaa", Name: "RTX 4090", MemUsedMB: 4000, MemTotalMB: 24000, Timestamp: fresh},
		{ID: 1, Name: "Intel GPU 0xe20b (xe)", MemUsedMB: 1000, MemTotalMB: 12000, Timestamp: fresh},
		{ID: 2, UUID: "no-total", MemTotalMB: 0, Timestamp: fresh},
		{ID: 3, UUID: "over", MemUsedMB: 9, MemTotalMB: 8, Timestamp: fresh},
		{ID: 4, UUID: "stale", MemUsedMB: 0, MemTotalMB: 8000, Timestamp: arbiterNow.Add(-time.Minute)},
		{ID: 5, UUID: "unstamped", MemUsedMB: 0, MemTotalMB: 8000},
	})

	devs, err := probe.Devices(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []router.Device{
		{ID: "GPU-aaaa", Name: "RTX 4090", TotalMB: 24000, FreeMB: 20000, Sampled: fresh, UUID: "GPU-aaaa"},
		{ID: "gpu1", Name: "Intel GPU 0xe20b (xe)", TotalMB: 12000, FreeMB: 11000, Sampled: fresh},
	}, devs)
}

// intelProbe is a probe whose host lists the given Intel GPU addresses.
func intelProbe(stats []perf.GpuStat, host []string, hostErr error) gpuProbe {
	p := fixedGpuProbe(stats)
	p.intelGPUs = func() ([]string, error) { return host, hostErr }
	return p
}

func probeByID(t *testing.T, p gpuProbe) map[string]router.Device {
	t.Helper()
	devs, err := p.Devices(context.Background())
	require.NoError(t, err)
	got := map[string]router.Device{}
	for _, d := range devs {
		got[d.ID] = d
	}
	return got
}

func ordinalOf(d router.Device) int {
	if d.Ordinal == nil {
		return -1
	}
	return *d.Ordinal
}

// The probe carries what placement needs to build a visibility variable: the
// vendor, the UUID and, for Intel, the rank by PCI address among all Intel
// GPUs, counted before stale cards are left out so an omitted card never
// shifts the rest.
func TestServer_ArbiterProbe_VendorAndIntelOrdinals(t *testing.T) {
	fresh := arbiterNow.Add(-time.Second)
	stats := []perf.GpuStat{
		{ID: 0, Vendor: perf.VendorIntel, UUID: "0000:09:00.0", MemTotalMB: 12000, Timestamp: fresh},
		{ID: 1, Vendor: perf.VendorNVIDIA, UUID: "GPU-n", MemTotalMB: 24000, Timestamp: fresh},
		{ID: 2, Vendor: perf.VendorIntel, UUID: "0000:03:00.0", MemTotalMB: 12000, Timestamp: arbiterNow.Add(-time.Hour)},
		{ID: 3, Vendor: perf.VendorIntel, UUID: "0000:0B:00.0", MemTotalMB: 12000, Timestamp: fresh},
	}
	got := probeByID(t, intelProbe(stats, []string{"0000:0b:00.0", "0000:03:00.0", "0000:09:00.0"}, nil))

	require.Len(t, got, 3, "the stale card is omitted")
	assert.Equal(t, 1, ordinalOf(got["0000:09:00.0"]), "03:00.0 ranks first although omitted")
	assert.Equal(t, 2, ordinalOf(got["0000:0B:00.0"]), "address case does not change the rank")
	assert.Equal(t, perf.VendorIntel, got["0000:09:00.0"].Vendor)
	assert.Equal(t, perf.VendorNVIDIA, got["GPU-n"].Vendor)
	assert.Equal(t, "GPU-n", got["GPU-n"].UUID)
	assert.Nil(t, got["GPU-n"].Ordinal, "only Intel devices get an ordinal")
}

// An Intel GPU the reading lacks (an i915 iGPU the xe collector cannot read,
// say) would shift every Level Zero ordinal, so no Intel device gets one; the
// same holds when the host list cannot be read or names a card twice over.
func TestServer_ArbiterProbe_IntelOrdinalsFailClosed(t *testing.T) {
	fresh := arbiterNow.Add(-time.Second)
	stats := []perf.GpuStat{
		{ID: 0, Vendor: perf.VendorIntel, UUID: "0000:03:00.0", MemTotalMB: 12000, Timestamp: fresh},
		{ID: 1, Vendor: perf.VendorIntel, UUID: "0000:09:00.0", MemTotalMB: 12000, Timestamp: fresh},
	}
	cases := []struct {
		name    string
		host    []string
		hostErr error
	}{
		{"i915 iGPU ahead of the xe cards", []string{"0000:00:02.0", "0000:03:00.0", "0000:09:00.0"}, nil},
		{"unlisted card in place of a read one", []string{"0000:00:02.0", "0000:09:00.0"}, nil},
		{"host lists fewer cards", []string{"0000:03:00.0"}, nil},
		{"host list read with an error", []string{"0000:03:00.0", "0000:09:00.0"}, assert.AnError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := probeByID(t, intelProbe(stats, tc.host, tc.hostErr))
			require.Len(t, got, 2)
			for id, d := range got {
				assert.Nil(t, d.Ordinal, "%s must have no ordinal", id)
			}
		})
	}

	got := probeByID(t, fixedGpuProbe(stats))
	assert.Nil(t, got["0000:03:00.0"].Ordinal, "no host lister, no ordinal")
}

func TestServer_ArbiterProbe_NoReadingIsError(t *testing.T) {
	_, err := gpuProbe{}.Devices(context.Background())
	assert.Error(t, err, "monitor disabled")

	_, err = fixedGpuProbe(nil).Devices(context.Background())
	assert.Error(t, err, "no snapshot yet")
}

func arbiterTestServer(t *testing.T, stats []perf.GpuStat) (*Server, *stubRouter, chan string) {
	t.Helper()
	warmed := make(chan string, 4)
	local := newStubRouter([]string{"hot", "cold"}, "")
	local.running = map[string]process.ProcessState{"hot": process.StateReady}
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		data, _ := swaputil.ReadContext(r.Context())
		warmed <- data.ModelID
		w.WriteHeader(http.StatusOK)
	}
	s := newTestServer(local, newStubRouter(nil, ""))
	s.arbiter = router.NewArbiter(fixedGpuProbe(stats))
	return s, local, warmed
}

func postAdmit(s *Server, body string) (*httptest.ResponseRecorder, map[string]any) {
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/arbiter/admit", strings.NewReader(body)))
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return w, resp
}

func TestServer_ArbiterAdmit_Statuses(t *testing.T) {
	s, _, warmed := arbiterTestServer(t, []perf.GpuStat{
		{ID: 0, UUID: "GPU-aaaa", MemUsedMB: 4000, MemTotalMB: 24000, Timestamp: arbiterNow},
	})

	w, resp := postAdmit(s, `{"model":"hot"}`)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "admitted", resp["admission"])
	assert.Equal(t, arbiterAPIVersion, resp["api"])

	w, resp = postAdmit(s, `{"model":"cold"}`)
	assert.Equal(t, http.StatusAccepted, w.Code)
	assert.Equal(t, "queued", resp["admission"])
	assert.EqualValues(t, 1, resp["position"])
	select {
	case m := <-warmed:
		assert.Equal(t, "cold", m)
	case <-time.After(5 * time.Second):
		t.Fatal("queued model was not warmed")
	}

	w, _ = postAdmit(s, `{"model":"nope"}`)
	assert.Equal(t, http.StatusNotFound, w.Code)

	w, _ = postAdmit(s, `not json`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// With no GPU reading, admission is closed for every model, ready or not,
// and nothing is loaded.
func TestServer_ArbiterAdmit_ProbeErrorClosed(t *testing.T) {
	s, _, warmed := arbiterTestServer(t, nil)

	for _, model := range []string{"hot", "cold"} {
		w, resp := postAdmit(s, `{"model":"`+model+`"}`)
		assert.Equal(t, http.StatusServiceUnavailable, w.Code, model)
		assert.Equal(t, "closed", resp["admission"], model)
	}
	assert.Empty(t, warmed)

	s.arbiter = nil
	w, _ := postAdmit(s, `{"model":"hot"}`)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "no arbiter")
}

func TestServer_ArbiterPool(t *testing.T) {
	s, _, _ := arbiterTestServer(t, []perf.GpuStat{
		{ID: 0, UUID: "GPU-aaaa", MemUsedMB: 4000, MemTotalMB: 24000, Timestamp: arbiterNow},
	})
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/arbiter/pool", nil))
	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		API       string                          `json:"api"`
		Devices   []router.Device                 `json:"devices"`
		Residents map[string]process.ProcessState `json:"residents"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, arbiterAPIVersion, resp.API)
	require.Len(t, resp.Devices, 1)
	assert.Equal(t, 20000, resp.Devices[0].FreeMB)
	assert.Equal(t, process.StateReady, resp.Residents["hot"])

	s.arbiter = router.NewArbiter(fixedGpuProbe(nil))
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/arbiter/pool", nil))
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}
