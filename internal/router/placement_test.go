package router

import (
	"testing"

	"github.com/mostlygeek/llama-swap/internal/perf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ordinal(i int) *int { return &i }

func TestRouter_VisibilityEnv(t *testing.T) {
	cases := []struct {
		name    string
		dev     Device
		want    []string
		wantErr error
	}{
		{"nvidia uses the UUID", Device{ID: "x", Vendor: perf.VendorNVIDIA, UUID: "GPU-1234"}, []string{"CUDA_VISIBLE_DEVICES=GPU-1234"}, nil},
		{"nvidia without UUID", Device{ID: "x", Vendor: perf.VendorNVIDIA}, nil, ErrUnknownVendor},
		{"amd uses a ROCm UUID", Device{ID: "x", Vendor: perf.VendorAMD, UUID: "GPU-0123456789abcdef"}, []string{"ROCR_VISIBLE_DEVICES=GPU-0123456789abcdef"}, nil},
		{"amd rocm-smi GUID is not a ROCm UUID", Device{ID: "x", Vendor: perf.VendorAMD, UUID: "12345"}, nil, ErrUnknownVendor},
		{"amd without UUID", Device{ID: "x", Vendor: perf.VendorAMD}, nil, ErrUnknownVendor},
		{"intel uses the PCI-order ordinal", Device{ID: "0000:03:00.0", Vendor: perf.VendorIntel, UUID: "0000:03:00.0", Ordinal: ordinal(1)},
			[]string{"ZE_ENABLE_PCI_ID_DEVICE_ORDER=1", "ZE_AFFINITY_MASK=1"}, nil},
		{"intel ordinal zero is valid", Device{ID: "0000:03:00.0", Vendor: perf.VendorIntel, Ordinal: ordinal(0)},
			[]string{"ZE_ENABLE_PCI_ID_DEVICE_ORDER=1", "ZE_AFFINITY_MASK=0"}, nil},
		{"intel without ordinal", Device{ID: "0000:03:00.0", Vendor: perf.VendorIntel, UUID: "0000:03:00.0"}, nil, ErrUnknownVendor},
		{"intel negative ordinal", Device{ID: "0000:03:00.0", Vendor: perf.VendorIntel, Ordinal: ordinal(-1)}, nil, ErrUnknownVendor},
		{"unknown vendor", Device{ID: "gpu0", Vendor: "", UUID: "u"}, nil, ErrUnknownVendor},
		{"other vendor", Device{ID: "gpu0", Vendor: "apple", UUID: "u"}, nil, ErrUnknownVendor},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := VisibilityEnv(tc.dev)
			if tc.wantErr != nil {
				assert.ErrorIs(t, err, tc.wantErr)
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
