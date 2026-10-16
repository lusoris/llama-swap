# Accelerator arbiter

Status: design, awaiting API decision (CUT-1632-11).

The arbiter decides on which accelerator a model loads and when a request
for it is admitted. It treats every accelerator on the host as one pool and
decides from memory it measures at runtime. It sits beside the matrix solver
(`matrix_solver.go`) and plugs into the existing `scheduler.Swapper` seam, so
the FIFO scheduler, the single run loop and the swap goroutine stay as they
are (see `design.md`).

## Rules it holds

- One dynamic pool. Any model may load on any device that fits it. Adding,
  removing or swapping a device needs no configuration change: devices come
  from `internal/hw` (inventory, vendor) and `internal/perf` (measured used
  and total memory per device).
- Placement comes from measured free memory only. No per-device model maps,
  no pins, no `capacity: 1`, no keep-warm, no infinite TTL. Existing upstream
  features with those shapes (`profiles.pins`, selector `pin`/`warm`) are left
  in place; the arbiter does not read them.
- A saturated device next to an idle one is a placement bug. Spread first
  (worst fit: the device with the most free memory that fits wins), evict
  only when no device fits.
- Admission waits for the physical model to report `ready`. A request for a
  model that is not loaded triggers the load and stays queued; it is never
  handed to an upstream that is still starting.
- Fail closed. A device whose memory cannot be read is absent from the pool
  (it is not "free"). A probe error refuses placement and admission with
  `ErrProbe`; the request gets that error, never a guessed placement.

## Footprint

A model's memory need is learned, not declared:

- After a load reaches `ready`, the arbiter records the measured used-memory
  delta on the device it chose. The footprint is keyed by model only, not by
  device, and keeps the largest delta seen.
- A model with no recorded footprint is placed on the device with the most
  measured free memory, and nothing else is co-placed on that device until
  the footprint is recorded (one-shot fence released by the first `ready`).
- Anti-thrash: a model stays resident at least `max(floor, 3 x measured load
  time)`, capped, before it is an eviction candidate (ported from qdo
  `compute_min_residency`).

## Failure classes (folded from the issue 945 failover design)

Folded from commit `1f83153` on
`claude/modelchain-failure-failover-design-xem9zp`; that branch is retired.

| Class | Examples | Arbiter reaction |
|---|---|---|
| Hard, target unusable | local start failure (`doSwap` error), process not ready 503, health check timeout | load failed: clear the fence, no footprint recorded, request gets the error |
| Probe | memory read fails or is stale | `ErrProbe`, device absent, admission closed |
| Not a failure | model cold (`stopped`), `starting`, queued behind a swap, TTL or admin unload, client cancel, shutdown | queue or ignore; never counts against the model |

Cold is not failed: a stopped model is loaded by the request, and the arbiter
never probes a local model by loading it. Health of peers (circuit breaker,
half-open probe, `X-Llamaswap-Error` header) stays in the failover selector
design for upstream issue 945; it is not part of placement.

## Exported API sketch

The package is `internal/`, so other repositories cannot import it. Consumers
(Aegis-OS lictor, praetor, epitrop, qdo) reach it over HTTP; the Go types
below are the in-process API and the shape of the JSON.

```go
// Device is one accelerator as measured at one instant.
type Device struct {
	ID      string    // stable key: UUID, else PCI address
	Vendor  string    // from internal/hw; selects the visibility env var
	TotalMB int
	FreeMB  int       // measured; never defaulted
	Sampled time.Time
}

// MemoryProbe reads the pool. An unreadable device is omitted; an error
// means the whole reading is unusable and admission closes.
type MemoryProbe interface {
	Devices(ctx context.Context) ([]Device, error)
}

// Resident is a loaded or loading model.
type Resident struct {
	Model    string
	Device   string
	State    process.ProcessState
	InFlight int
	Since    time.Time
}

type Placement struct {
	Model  string
	Device string   // chosen device ID
	Evict  []string // models to stop first, possibly empty
	NeedMB int      // footprint used for the decision; 0 = unmeasured, fenced
}

var (
	ErrProbe = errors.New("arbiter: accelerator memory unreadable") // fail closed
	ErrNoFit = errors.New("arbiter: no device fits")                // request stays queued
)

type Arbiter struct{ /* probe, footprints, residency, fences */ }

func NewArbiter(probe MemoryProbe, opts Options) *Arbiter

// Place picks a device and evictions for model given the residents.
func (a *Arbiter) Place(ctx context.Context, model string, residents []Resident) (Placement, error)

// Loaded records a model reaching ready on dev; updates footprint and fence.
func (a *Arbiter) Loaded(model, dev string, before, after Device, loadTime time.Duration)

// Admission is the verdict for one request.
type Admission int

const (
	Admitted Admission = iota // physical model ready
	Queued                    // load triggered or in flight
	Closed                    // ErrProbe or hard load failure
)

func (a *Arbiter) Admit(ctx context.Context, model string, state func(string) (process.ProcessState, bool)) (Admission, error)
```

HTTP surface (read and admit only):

- `GET /api/arbiter/pool` returns `[]Device` plus residents.
- `POST /api/arbiter/admit {"model": "..."}` returns 200 `Admitted`, 202
  `Queued` with queue position, 503 `Closed` with the reason.

## Wiring

- `doSwap` asks `Place` for the device and passes it to the process start,
  which sets the vendor's visibility variable (`CUDA_VISIBLE_DEVICES`,
  `HIP_VISIBLE_DEVICES`, `ONEAPI_DEVICE_SELECTOR`, `GGML_VK_VISIBLE_DEVICES`)
  for that one start. The model's `cmd` stays device-free.
- With a matrix configured, the solver still limits which evictions are
  allowed; the arbiter picks among them by measured memory.

## Tests (planted)

- Saturated device with an idle peer: the model is placed on the idle one.
- Unloaded model: the request is `Queued`, a load is started, the request is
  admitted only after `ready`.
- Probe error: `Closed` with `ErrProbe`; no placement, no load.
- Unreadable device: absent from the pool, never chosen.
- Each test is run once against code without the fix and must fail there.
