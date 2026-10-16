package router

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/mostlygeek/llama-swap/internal/process"
)

// The arbiter treats every accelerator on the host as one pool. It places a
// model on the device with the most measured free memory that fits the
// model's learned footprint, and admits a request only once the physical
// model reports ready. It reads no per-device model map and no pin; see
// arbiter.md for the rules it holds.

var (
	// ErrProbe closes placement and admission: the pool could not be measured.
	ErrProbe = errors.New("arbiter: accelerator memory unreadable")
	// ErrNoFit means no measured device has room for the model's footprint.
	ErrNoFit = errors.New("arbiter: no device fits")
	// ErrUnknownModel means no local process serves the model.
	ErrUnknownModel = errors.New("arbiter: unknown model")
)

// Device is one accelerator as measured at one instant.
type Device struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	TotalMB int       `json:"total_mb"`
	FreeMB  int       `json:"free_mb"`
	Sampled time.Time `json:"sampled"`
}

// MemoryProbe measures the pool. A device whose memory cannot be read is
// omitted; an error means the whole reading is unusable.
type MemoryProbe interface {
	Devices(ctx context.Context) ([]Device, error)
}

// ModelLoader is the arbiter's view of the local processes: the state of the
// physical model, and a way to start loading it. Warm calls done once the
// load attempt ends, with its error.
type ModelLoader interface {
	ModelState(model string) (process.ProcessState, bool)
	Warm(model string, done func(error))
}

// Placement is the device chosen for a model. NeedMB is the footprint the
// choice rested on; 0 means unmeasured, and the device is fenced until the
// model's first ready.
type Placement struct {
	Model  string `json:"model"`
	Device string `json:"device"`
	NeedMB int    `json:"need_mb"`
}

// Admission is the verdict for one request.
type Admission string

const (
	Admitted Admission = "admitted" // physical model ready
	Queued   Admission = "queued"   // load triggered or in flight
	Closed   Admission = "closed"   // probe error or unknown model
)

// Verdict is an admission with the request's place in the load queue
// (1-based, only for Queued).
type Verdict struct {
	Admission Admission `json:"admission"`
	Position  int       `json:"position,omitempty"`
}

type Arbiter struct {
	probe MemoryProbe

	mu         sync.Mutex
	footprints map[string]int    // model -> largest measured footprint, MB
	fences     map[string]string // unmeasured model -> device held for it
	queue      []string          // models warming, in arrival order
}

func NewArbiter(probe MemoryProbe) *Arbiter {
	return &Arbiter{
		probe:      probe,
		footprints: map[string]int{},
		fences:     map[string]string{},
	}
}

// Pool returns the measured devices. An empty reading is ErrProbe: nothing
// measured is never read as free memory.
func (a *Arbiter) Pool(ctx context.Context) ([]Device, error) {
	devs, err := a.probe.Devices(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrProbe, err)
	}
	if len(devs) == 0 {
		return nil, fmt.Errorf("%w: no device measured", ErrProbe)
	}
	return devs, nil
}

// Place picks the device for model: of the devices not fenced for another
// model that fit its footprint, the one with the most free memory (worst
// fit), ties broken by ID. An unmeasured model fences the device it gets.
func (a *Arbiter) Place(ctx context.Context, model string) (Placement, error) {
	devs, err := a.Pool(ctx)
	if err != nil {
		return Placement{}, err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	need := a.footprints[model]
	var best *Device
	for i := range devs {
		d := &devs[i]
		if a.fencedFor(d.ID, model) || d.FreeMB < need || d.FreeMB <= 0 {
			continue
		}
		if best == nil || d.FreeMB > best.FreeMB || (d.FreeMB == best.FreeMB && d.ID < best.ID) {
			best = d
		}
	}
	if best == nil {
		return Placement{}, fmt.Errorf("%w: %s needs %d MB", ErrNoFit, model, need)
	}
	if need == 0 {
		a.fences[model] = best.ID
	}
	return Placement{Model: model, Device: best.ID, NeedMB: need}, nil
}

// fencedFor reports whether dev is held for an unmeasured model other than
// model. Callers hold a.mu.
func (a *Arbiter) fencedFor(dev, model string) bool {
	for m, d := range a.fences {
		if d == dev && m != model {
			return true
		}
	}
	return false
}

// Loaded records that model reached ready on device using usedDeltaMB of its
// memory. The footprint keeps the largest delta seen, and the model's fence
// is released.
func (a *Arbiter) Loaded(model, device string, usedDeltaMB int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.fences, model)
	if usedDeltaMB > a.footprints[model] {
		a.footprints[model] = usedDeltaMB
	}
}

// Admit decides one request for model. A probe error closes admission before
// anything else; a ready model is admitted; any other state starts one
// warmup and queues the request.
func (a *Arbiter) Admit(ctx context.Context, model string, loader ModelLoader) (Verdict, error) {
	if _, err := a.Pool(ctx); err != nil {
		return Verdict{Admission: Closed}, err
	}
	state, ok := loader.ModelState(model)
	if !ok {
		return Verdict{Admission: Closed}, fmt.Errorf("%w: %s", ErrUnknownModel, model)
	}
	if state == process.StateShutdown {
		return Verdict{Admission: Closed}, fmt.Errorf("arbiter: %s is shut down", model)
	}

	a.mu.Lock()
	if state == process.StateReady {
		a.dequeue(model)
		a.mu.Unlock()
		return Verdict{Admission: Admitted}, nil
	}
	pos := slices.Index(a.queue, model) + 1
	warm := pos == 0
	if warm {
		a.queue = append(a.queue, model)
		pos = len(a.queue)
	}
	a.mu.Unlock()

	if warm {
		loader.Warm(model, func(error) {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.dequeue(model)
		})
	}
	return Verdict{Admission: Queued, Position: pos}, nil
}

// dequeue removes model from the load queue. Callers hold a.mu.
func (a *Arbiter) dequeue(model string) {
	a.queue = slices.DeleteFunc(a.queue, func(m string) bool { return m == model })
}
