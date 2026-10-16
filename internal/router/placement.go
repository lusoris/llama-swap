package router

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/perf"
)

// Placement visibility: once a device is placed, the model process must see
// that device only. The start path that sets this env is not wired yet; see
// arbiter.md.

// ErrUnknownVendor means the placed device's vendor is unknown, or its
// identity is not enough to build a visibility variable. The model is not
// started: running it unrestricted would ignore the placement.
var ErrUnknownVendor = errors.New("placement: cannot build device visibility")

// VisibilityEnv returns the KEY=VALUE entries that restrict a process to
// device d. Each vendor accepts only an identity that names one device for
// certain; anything else fails closed. Vendor facts, checked 2026-10-16:
//
//   - NVIDIA: CUDA_VISIBLE_DEVICES takes the full nvidia-smi UUID.
//   - AMD: ROCR_VISIBLE_DEVICES takes a ROCm UUID ("GPU-..."). It also takes
//     an index, but that is ROCr's enumeration order, which the rocm-smi card
//     number is not guaranteed to match, so an index is never used.
//   - Intel Level Zero: ZE_AFFINITY_MASK takes an ordinal only, so
//     ZE_ENABLE_PCI_ID_DEVICE_ORDER=1 pins the order the ordinal counts in.
func VisibilityEnv(d Device) ([]string, error) {
	switch d.Vendor {
	case perf.VendorNVIDIA:
		if d.UUID == "" {
			return nil, fmt.Errorf("%w: nvidia device %s has no UUID", ErrUnknownVendor, d.ID)
		}
		return []string{"CUDA_VISIBLE_DEVICES=" + d.UUID}, nil
	case perf.VendorAMD:
		if !strings.HasPrefix(d.UUID, "GPU-") {
			return nil, fmt.Errorf("%w: amd device %s has no ROCm UUID", ErrUnknownVendor, d.ID)
		}
		return []string{"ROCR_VISIBLE_DEVICES=" + d.UUID}, nil
	case perf.VendorIntel:
		if d.Ordinal == nil || *d.Ordinal < 0 {
			return nil, fmt.Errorf("%w: intel device %s has no known ordinal", ErrUnknownVendor, d.ID)
		}
		return []string{
			"ZE_ENABLE_PCI_ID_DEVICE_ORDER=1",
			"ZE_AFFINITY_MASK=" + strconv.Itoa(*d.Ordinal),
		}, nil
	default:
		return nil, fmt.Errorf("%w: device %s has vendor %q", ErrUnknownVendor, d.ID, d.Vendor)
	}
}
