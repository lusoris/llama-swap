package router

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeProbe struct {
	mu   sync.Mutex
	devs []Device
	err  error
}

func (p *fakeProbe) Devices(context.Context) ([]Device, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Device(nil), p.devs...), p.err
}

type fakeLoader struct {
	mu     sync.Mutex
	states map[string]process.ProcessState
	warmed []string
	done   map[string]func(error)
}

func newFakeLoader(states map[string]process.ProcessState) *fakeLoader {
	return &fakeLoader{states: states, done: map[string]func(error){}}
}

func (l *fakeLoader) ModelState(model string) (process.ProcessState, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.states[model]
	return s, ok
}

func (l *fakeLoader) Warm(model string, done func(error)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warmed = append(l.warmed, model)
	l.done[model] = done
}

func (l *fakeLoader) set(model string, s process.ProcessState) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.states[model] = s
}

func (l *fakeLoader) finish(model string, err error) {
	l.mu.Lock()
	done := l.done[model]
	l.mu.Unlock()
	done(err)
}

// A device listed first but nearly full must not win over an idle peer.
func TestArbiter_Place_SaturatedDevicePlacesOnIdlePeer(t *testing.T) {
	probe := &fakeProbe{devs: []Device{
		{ID: "gpu-a", TotalMB: 24000, FreeMB: 500},
		{ID: "gpu-b", TotalMB: 24000, FreeMB: 23000},
	}}
	a := NewArbiter(probe)
	a.Loaded("m", "gpu-b", 8000)

	p, err := a.Place(context.Background(), "m")
	require.NoError(t, err)
	assert.Equal(t, "gpu-b", p.Device)
	assert.Equal(t, 8000, p.NeedMB)
}

// Worst fit spreads: of two devices that both fit, the one with more free
// memory wins, whatever order the probe lists them in.
func TestArbiter_Place_WorstFitSpreads(t *testing.T) {
	probe := &fakeProbe{devs: []Device{
		{ID: "gpu-a", TotalMB: 16000, FreeMB: 12000},
		{ID: "gpu-b", TotalMB: 24000, FreeMB: 14000},
	}}
	a := NewArbiter(probe)
	a.Loaded("m", "gpu-a", 4000)

	p, err := a.Place(context.Background(), "m")
	require.NoError(t, err)
	assert.Equal(t, "gpu-b", p.Device)
}

func TestArbiter_Place_NoDeviceFits(t *testing.T) {
	probe := &fakeProbe{devs: []Device{
		{ID: "gpu-a", TotalMB: 24000, FreeMB: 7999},
		{ID: "gpu-b", TotalMB: 8000, FreeMB: 4000},
	}}
	a := NewArbiter(probe)
	a.Loaded("m", "gpu-a", 8000)

	_, err := a.Place(context.Background(), "m")
	assert.ErrorIs(t, err, ErrNoFit)
}

func TestArbiter_Place_ProbeErrorClosed(t *testing.T) {
	probe := &fakeProbe{
		devs: []Device{{ID: "gpu-a", TotalMB: 24000, FreeMB: 24000}},
		err:  errors.New("nvidia-smi: exit 9"),
	}
	_, err := NewArbiter(probe).Place(context.Background(), "m")
	assert.ErrorIs(t, err, ErrProbe)
}

// An empty pool means nothing was measured; it is never read as free memory.
func TestArbiter_Place_EmptyPoolClosed(t *testing.T) {
	_, err := NewArbiter(&fakeProbe{}).Place(context.Background(), "m")
	assert.ErrorIs(t, err, ErrProbe)
}

// A model without a measured footprint takes the device with the most free
// memory and fences it: nothing else is co-placed there until its first
// ready reports the footprint.
func TestArbiter_Place_UnmeasuredModelFencesDevice(t *testing.T) {
	probe := &fakeProbe{devs: []Device{
		{ID: "gpu-a", TotalMB: 16000, FreeMB: 10000},
		{ID: "gpu-b", TotalMB: 24000, FreeMB: 20000},
	}}
	a := NewArbiter(probe)

	px, err := a.Place(context.Background(), "x")
	require.NoError(t, err)
	assert.Equal(t, "gpu-b", px.Device)
	assert.Equal(t, 0, px.NeedMB)

	py, err := a.Place(context.Background(), "y")
	require.NoError(t, err)
	assert.Equal(t, "gpu-a", py.Device, "gpu-b is fenced while x loads unmeasured")

	a.Loaded("x", "gpu-b", 6000)
	probe.devs[1].FreeMB = 14000
	pz, err := a.Place(context.Background(), "z")
	require.NoError(t, err)
	assert.Equal(t, "gpu-b", pz.Device, "x's ready released the fence")
}

// A request for a model that is not loaded triggers one warmup and stays
// queued; it is admitted only once the physical model reports ready.
func TestArbiter_Admit_UnloadedModelQueuedUntilReady(t *testing.T) {
	probe := &fakeProbe{devs: []Device{{ID: "gpu-a", TotalMB: 24000, FreeMB: 20000}}}
	loader := newFakeLoader(map[string]process.ProcessState{
		"m": process.StateStopped,
		"n": process.StateStopped,
	})
	a := NewArbiter(probe)
	ctx := context.Background()

	v, err := a.Admit(ctx, "m", loader)
	require.NoError(t, err)
	assert.Equal(t, Verdict{Admission: Queued, Position: 1}, v)

	loader.set("m", process.StateStarting)
	v, err = a.Admit(ctx, "m", loader)
	require.NoError(t, err)
	assert.Equal(t, Verdict{Admission: Queued, Position: 1}, v)

	v, err = a.Admit(ctx, "n", loader)
	require.NoError(t, err)
	assert.Equal(t, Verdict{Admission: Queued, Position: 2}, v)
	assert.Equal(t, []string{"m", "n"}, loader.warmed, "one warmup per model")

	loader.set("m", process.StateReady)
	loader.finish("m", nil)
	v, err = a.Admit(ctx, "m", loader)
	require.NoError(t, err)
	assert.Equal(t, Verdict{Admission: Admitted}, v)

	v, err = a.Admit(ctx, "n", loader)
	require.NoError(t, err)
	assert.Equal(t, Verdict{Admission: Queued, Position: 1}, v, "n moved up once m left the queue")
}

// A probe error closes admission even for a model that is ready, and starts
// no load.
func TestArbiter_Admit_ProbeErrorClosed(t *testing.T) {
	probe := &fakeProbe{err: errors.New("no GPU reading")}
	loader := newFakeLoader(map[string]process.ProcessState{
		"ready": process.StateReady,
		"cold":  process.StateStopped,
	})
	a := NewArbiter(probe)

	for _, model := range []string{"ready", "cold"} {
		v, err := a.Admit(context.Background(), model, loader)
		assert.ErrorIs(t, err, ErrProbe, model)
		assert.Equal(t, Verdict{Admission: Closed}, v, model)
	}
	assert.Empty(t, loader.warmed)
}

func TestArbiter_Admit_UnknownModelClosed(t *testing.T) {
	probe := &fakeProbe{devs: []Device{{ID: "gpu-a", TotalMB: 24000, FreeMB: 20000}}}
	v, err := NewArbiter(probe).Admit(context.Background(), "nope", newFakeLoader(map[string]process.ProcessState{}))
	assert.ErrorIs(t, err, ErrUnknownModel)
	assert.Equal(t, Verdict{Admission: Closed}, v)
}

// A failed warmup leaves the queue, so the next request starts a new load
// instead of waiting on one nobody is running.
func TestArbiter_Admit_FailedWarmupRequeues(t *testing.T) {
	probe := &fakeProbe{devs: []Device{{ID: "gpu-a", TotalMB: 24000, FreeMB: 20000}}}
	loader := newFakeLoader(map[string]process.ProcessState{"m": process.StateStopped})
	a := NewArbiter(probe)
	ctx := context.Background()

	_, err := a.Admit(ctx, "m", loader)
	require.NoError(t, err)
	loader.finish("m", errors.New("health check timed out"))

	v, err := a.Admit(ctx, "m", loader)
	require.NoError(t, err)
	assert.Equal(t, Verdict{Admission: Queued, Position: 1}, v)
	assert.Equal(t, []string{"m", "m"}, loader.warmed)
}
