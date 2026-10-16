//go:build linux

package perf

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type xeFixtureRegion struct {
	class    uint16
	instance uint16
	total    uint64
	used     uint64
}

// xeFixtureRegions builds a drm_xe_query_mem_regions buffer as the xe driver
// returns it from DRM_XE_DEVICE_QUERY_MEM_REGIONS.
func xeFixtureRegions(regions ...xeFixtureRegion) []byte {
	buf := make([]byte, xeMemRegionsHeaderSize+len(regions)*xeMemRegionSize)
	binary.LittleEndian.PutUint32(buf[0:], uint32(len(regions)))
	for i, r := range regions {
		off := xeMemRegionsHeaderSize + i*xeMemRegionSize
		binary.LittleEndian.PutUint16(buf[off+0:], r.class)
		binary.LittleEndian.PutUint16(buf[off+2:], r.instance)
		binary.LittleEndian.PutUint32(buf[off+4:], 4096)
		binary.LittleEndian.PutUint64(buf[off+8:], r.total)
		binary.LittleEndian.PutUint64(buf[off+16:], r.used)
	}
	return buf
}

const mib = 1024 * 1024

func TestPerf_ParseXeMemRegions_SumsVramTilesIgnoresSysmem(t *testing.T) {
	buf := xeFixtureRegions(
		xeFixtureRegion{class: xeMemRegionClassSysmem, instance: 0, total: 64 * 1024 * mib, used: 32 * 1024 * mib},
		xeFixtureRegion{class: xeMemRegionClassVram, instance: 1, total: 12 * 1024 * mib, used: 3 * 1024 * mib},
		xeFixtureRegion{class: xeMemRegionClassVram, instance: 2, total: 12 * 1024 * mib, used: 1024 * mib},
	)

	used, total, err := parseXeMemRegions(buf)
	require.NoError(t, err)
	assert.Equal(t, uint64(4*1024*mib), used)
	assert.Equal(t, uint64(24*1024*mib), total)
}

func TestPerf_ParseXeMemRegions_RefusesBadBuffers(t *testing.T) {
	good := xeFixtureRegions(xeFixtureRegion{class: xeMemRegionClassVram, total: 8 * 1024 * mib, used: mib})

	cases := map[string][]byte{
		"empty":           nil,
		"short header":    good[:4],
		"truncated":       good[:len(good)-1],
		"no vram":         xeFixtureRegions(xeFixtureRegion{class: xeMemRegionClassSysmem, total: 8 * 1024 * mib}),
		"used over total": xeFixtureRegions(xeFixtureRegion{class: xeMemRegionClassVram, total: mib, used: 2 * mib}),
	}
	for name, buf := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := parseXeMemRegions(buf)
			assert.Error(t, err)
		})
	}
}

// xeFixtureCard lays out /sys/class/drm/<card>/device the way the kernel
// does: vendor and device files, a driver symlink and the render node name.
func xeFixtureCard(t *testing.T, sysRoot, card, pciAddr, vendor, device, driver, render string) {
	t.Helper()
	pciDir := filepath.Join(sysRoot, "devices", "pci0000:00", pciAddr)
	require.NoError(t, os.MkdirAll(filepath.Join(pciDir, "drm", render), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(pciDir, "vendor"), []byte(vendor+"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(pciDir, "device"), []byte(device+"\n"), 0o644))
	drvDir := filepath.Join(sysRoot, "bus", "pci", "drivers", driver)
	require.NoError(t, os.MkdirAll(drvDir, 0o755))
	require.NoError(t, os.Symlink(drvDir, filepath.Join(pciDir, "driver")))

	cardDir := filepath.Join(sysRoot, "class", "drm", card)
	require.NoError(t, os.MkdirAll(cardDir, 0o755))
	require.NoError(t, os.Symlink(pciDir, filepath.Join(cardDir, "device")))
}

func TestPerf_ReadXeDevices_MeasuredOnlyUnreadableOmitted(t *testing.T) {
	sysRoot := t.TempDir()
	xeFixtureCard(t, sysRoot, "card0", "0000:01:00.0", "0x1002", "0x7551", "amdgpu", "renderD128")
	xeFixtureCard(t, sysRoot, "card1", "0000:03:00.0", "0x8086", "0xe20b", "xe", "renderD129")
	xeFixtureCard(t, sysRoot, "card2", "0000:04:00.0", "0x8086", "0xe211", "xe", "renderD130")
	xeFixtureCard(t, sysRoot, "card3", "0000:05:00.0", "0x8086", "0x56a5", "i915", "renderD131")

	var queried []string
	query := func(node string) ([]byte, error) {
		queried = append(queried, node)
		switch node {
		case "/dev/dri/renderD129":
			return xeFixtureRegions(
				xeFixtureRegion{class: xeMemRegionClassSysmem, total: 64 * 1024 * mib, used: 10 * 1024 * mib},
				xeFixtureRegion{class: xeMemRegionClassVram, instance: 1, total: 12 * 1024 * mib, used: 3 * 1024 * mib},
			), nil
		default:
			return nil, errors.New("permission denied")
		}
	}

	stats := readXeDevices(sysRoot, "/dev", query)

	// The amdgpu and i915 cards are never queried; the second xe card fails its
	// query and is left out instead of being reported with a guessed size.
	assert.Equal(t, []string{"/dev/dri/renderD129", "/dev/dri/renderD130"}, queried)
	require.Len(t, stats, 1)
	s := stats[0]
	assert.Equal(t, 1, s.ID)
	assert.Equal(t, "0000:03:00.0", s.UUID)
	assert.Equal(t, "Intel GPU 0xe20b (xe)", s.Name)
	assert.Equal(t, 3*1024, s.MemUsedMB)
	assert.Equal(t, 12*1024, s.MemTotalMB)
	assert.InDelta(t, 25.0, s.MemUtilPct, 0.001)
}

func TestPerf_ReadXeDevices_NoDrmClass(t *testing.T) {
	stats := readXeDevices(t.TempDir(), "/dev", func(string) ([]byte, error) {
		t.Fatal("query must not run without an xe device")
		return nil, nil
	})
	assert.Empty(t, stats)
}

// TestPerf_ReadSysfs_RealXeDevice reads the host's xe devices through the real
// ioctl. It skips when no xe render node can be opened (no device, or a
// container that sees the host's sysfs without /dev/dri), unless
// LLAMA_SWAP_REQUIRE_XE=1 is set, which turns that into a failure for runs on
// Intel hosts.
func TestPerf_ReadSysfs_RealXeDevice(t *testing.T) {
	matches, _ := filepath.Glob("/sys/class/drm/card*/device/driver")
	var reason error = errors.New("no xe device under /sys/class/drm")
	usable := false
	for _, m := range matches {
		dst, err := os.Readlink(m)
		if err != nil || filepath.Base(dst) != "xe" {
			continue
		}
		render := xeRenderNode(filepath.Dir(m))
		if render == "" {
			reason = errors.New("xe device without a render node")
			continue
		}
		f, err := os.OpenFile(filepath.Join("/dev/dri", render), os.O_RDWR, 0)
		if err != nil {
			reason = err
			continue
		}
		f.Close()
		usable = true
	}
	if !usable {
		if os.Getenv("LLAMA_SWAP_REQUIRE_XE") == "1" {
			t.Fatalf("LLAMA_SWAP_REQUIRE_XE=1 but no xe render node is readable: %v", reason)
		}
		t.Skipf("no readable xe render node: %v", reason)
	}

	stats, err := readSysfs()
	require.NoError(t, err)
	require.NotEmpty(t, stats)
	for _, s := range stats {
		t.Logf("xe device %d %s %s: used %d MiB of %d MiB", s.ID, s.UUID, s.Name, s.MemUsedMB, s.MemTotalMB)
		assert.Positive(t, s.MemTotalMB)
		assert.LessOrEqual(t, s.MemUsedMB, s.MemTotalMB)
	}
}
