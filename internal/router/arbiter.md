# Accelerator arbiter

Status: core implemented (`arbiter.go`, `internal/server/arbiter.go`):
placement, footprint and fence bookkeeping, admission, and the HTTP surface.
`VisibilityEnv` (`placement.go`) builds the per-vendor visibility variable
for a placed device. Not yet wired: `doSwap` does not ask `Place` for a
device, and process starts set no visibility variable; that is the next
change. Until then, which device a model actually lands on still follows its
`cmd`.

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

## In-process API

The package is `internal/`, so other repositories cannot import it. Consumers
(Aegis-OS lictor, praetor, epitrop, qdo) reach it over HTTP; the Go types
below are the in-process API and the shape of the JSON.

```go
type MemoryProbe interface {
	Devices(ctx context.Context) ([]Device, error) // unreadable device omitted
}
type ModelLoader interface {
	ModelState(model string) (process.ProcessState, bool)
	Warm(model string, done func(error))
}

func NewArbiter(probe MemoryProbe) *Arbiter
func (a *Arbiter) Pool(ctx context.Context) ([]Device, error)        // empty or error: ErrProbe
func (a *Arbiter) Place(ctx context.Context, model string) (Placement, error) // ErrProbe, ErrNoFit
func (a *Arbiter) Loaded(model, device string, usedDeltaMB int)
func (a *Arbiter) Admit(ctx context.Context, model string, l ModelLoader) (Verdict, error)
```

Evictions stay with the existing `scheduler.Swapper`; `Place` only picks
among devices that already have room.

HTTP surface (read and admit only):

Every response carries `"api": "arbiter/v1"`.

- `GET /api/arbiter/pool` returns `devices` (measured, fresh within three
  `performance.every` intervals) and `residents` (model states); 503 when
  nothing is measured.
- `POST /api/arbiter/admit {"model": "..."}` returns 200 `admitted`, 202
  `queued` with `position`, 503 `closed` with the reason, 404 for an unknown
  model.

## Wiring (next change)

- `doSwap` asks `Place` for the device and passes `VisibilityEnv` of it to
  the process start for that one start. The model's `cmd` stays device-free.
- `VisibilityEnv` names one device for certain or fails closed with
  `ErrUnknownVendor`:
  - NVIDIA: `CUDA_VISIBLE_DEVICES=<nvidia-smi UUID>`.
  - AMD: `ROCR_VISIBLE_DEVICES=<ROCm UUID "GPU-...">`. The ROCr index is not
    used: its order is not guaranteed to match the rocm-smi card number.
  - Intel: `ZE_ENABLE_PCI_ID_DEVICE_ORDER=1` and `ZE_AFFINITY_MASK=<ordinal>`.
    The probe ranks Intel GPUs by PCI address, and only when every Intel
    display-class PCI device on the host is in the reading. A GPU the reading
    lacks (an i915 iGPU, say) would shift the Level Zero ordinals, so then
    no Intel device gets an ordinal.
- Until the collectors report a vendor, every device has an empty vendor and
  placement refuses to start anything.
- With a matrix configured, the solver still limits which evictions are
  allowed; the arbiter picks among them by measured memory.

## Tests (planted)

- Saturated device with an idle peer: the model is placed on the idle one.
- Unloaded model: the request is `Queued`, a load is started, the request is
  admitted only after `ready`.
- Probe error: `Closed` with `ErrProbe`; no placement, no load.
- Unreadable device: absent from the pool, never chosen.
- Each test is run once against code without the fix and must fail there.
