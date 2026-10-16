//go:build linux

package perf

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Intel xe driver memory query, from include/uapi/drm/xe_drm.h:
//
//	DRM_IOCTL_XE_DEVICE_QUERY = DRM_IOWR(DRM_COMMAND_BASE + DRM_XE_DEVICE_QUERY, struct drm_xe_device_query)
//	DRM_XE_DEVICE_QUERY_MEM_REGIONS returns struct drm_xe_query_mem_regions:
//	  { u32 num_mem_regions; u32 pad; struct drm_xe_mem_region mem_regions[]; }
//	struct drm_xe_mem_region (88 bytes):
//	  { u16 mem_class; u16 instance; u32 min_page_size; u64 total_size; u64 used;
//	    u64 cpu_visible_size; u64 cpu_visible_used; u64 reserved[6]; }
//
// The driver fills used for VRAM regions without a capability check, so an
// unprivileged process that can open the render node reads it. Multi-tile
// devices report one VRAM region per tile; they are summed. The ioctl number
// uses the generic _IOC encoding (x86, arm64, riscv); on an architecture with
// a different encoding the ioctl fails and the device is omitted.
const (
	xeIoctlDeviceQuery       = 0xc0286440
	xeDeviceQueryMemRegions  = 1
	xeMemRegionsHeaderSize   = 8
	xeMemRegionSize          = 88
	xeMemRegionClassSysmem   = 0
	xeMemRegionClassVram     = 1
	xeMemRegionsMaxQuerySize = 1 << 16

	intelPCIVendor = "0x8086"
)

type xeDeviceQuery struct {
	Extensions uint64
	Query      uint32
	Size       uint32
	Data       uint64
	Reserved   [2]uint64
}

// xeQueryFunc returns the raw drm_xe_query_mem_regions buffer of a render node.
type xeQueryFunc func(renderNode string) ([]byte, error)

// parseXeMemRegions sums used and total bytes over the VRAM regions of a
// drm_xe_query_mem_regions buffer. A buffer without VRAM is an error.
func parseXeMemRegions(buf []byte) (used, total uint64, err error) {
	if len(buf) < xeMemRegionsHeaderSize {
		return 0, 0, fmt.Errorf("xe mem regions: buffer of %d bytes has no header", len(buf))
	}
	n := int(binary.LittleEndian.Uint32(buf[0:]))
	if need := xeMemRegionsHeaderSize + n*xeMemRegionSize; n < 0 || len(buf) < need {
		return 0, 0, fmt.Errorf("xe mem regions: %d regions need %d bytes, got %d", n, need, len(buf))
	}

	vram := 0
	for i := range n {
		r := buf[xeMemRegionsHeaderSize+i*xeMemRegionSize:]
		if binary.LittleEndian.Uint16(r[0:]) != xeMemRegionClassVram {
			continue
		}
		vram++
		total += binary.LittleEndian.Uint64(r[8:])
		used += binary.LittleEndian.Uint64(r[16:])
	}
	if vram == 0 || total == 0 {
		return 0, 0, errors.New("xe mem regions: no VRAM region")
	}
	if used > total {
		return 0, 0, fmt.Errorf("xe mem regions: used %d exceeds total %d", used, total)
	}
	return used, total, nil
}

// readXeDevices lists the xe driven Intel GPUs under sysRoot/class/drm and
// measures each one through query. A device whose memory cannot be read is
// left out rather than reported with a guessed size.
func readXeDevices(sysRoot, devRoot string, query xeQueryFunc) []GpuStat {
	cards, _ := filepath.Glob(filepath.Join(sysRoot, "class", "drm", "card*"))

	type card struct {
		num int
		dir string
	}
	var found []card
	for _, dir := range cards {
		num, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(dir), "card"))
		if err != nil {
			continue // connector entries such as card0-DP-1
		}
		found = append(found, card{num: num, dir: dir})
	}
	slices.SortFunc(found, func(a, b card) int { return a.num - b.num })

	stats := make([]GpuStat, 0, len(found))
	for _, c := range found {
		dev := filepath.Join(c.dir, "device")
		vendor, err := os.ReadFile(filepath.Join(dev, "vendor"))
		if err != nil || strings.TrimSpace(string(vendor)) != intelPCIVendor {
			continue
		}
		driver, err := os.Readlink(filepath.Join(dev, "driver"))
		if err != nil || filepath.Base(driver) != "xe" {
			continue
		}
		render := xeRenderNode(dev)
		if render == "" {
			continue
		}

		buf, err := query(filepath.Join(devRoot, "dri", render))
		if err != nil {
			continue
		}
		used, total, err := parseXeMemRegions(buf)
		if err != nil {
			continue
		}

		pciDevice, _ := os.ReadFile(filepath.Join(dev, "device"))
		uuid := filepath.Base(dev)
		if resolved, err := filepath.EvalSymlinks(dev); err == nil {
			uuid = filepath.Base(resolved)
		}

		stats = append(stats, GpuStat{
			Timestamp:  time.Now(),
			ID:         c.num,
			Name:       fmt.Sprintf("Intel GPU %s (xe)", strings.TrimSpace(string(pciDevice))),
			UUID:       uuid,
			MemUsedMB:  int(used >> 20),
			MemTotalMB: int(total >> 20),
			MemUtilPct: float64(used) / float64(total) * 100,
		})
	}
	return stats
}

func xeRenderNode(deviceDir string) string {
	entries, err := os.ReadDir(filepath.Join(deviceDir, "drm"))
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "renderD") {
			return e.Name()
		}
	}
	return ""
}

// xeQueryMemRegions issues DRM_XE_DEVICE_QUERY_MEM_REGIONS on a render node:
// once with size 0 to learn the buffer size, then with the buffer.
func xeQueryMemRegions(renderNode string) ([]byte, error) {
	f, err := os.OpenFile(renderNode, os.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	q := xeDeviceQuery{Query: xeDeviceQueryMemRegions}
	if err := xeIoctl(f, &q); err != nil {
		return nil, fmt.Errorf("xe query size: %w", err)
	}
	if q.Size < xeMemRegionsHeaderSize || q.Size > xeMemRegionsMaxQuerySize {
		return nil, fmt.Errorf("xe query size %d out of range", q.Size)
	}

	buf := make([]byte, q.Size)
	var pin runtime.Pinner
	pin.Pin(&buf[0])
	defer pin.Unpin()
	// SAFETY: buf is pinned until return, so the address the kernel writes
	// through stays valid; the kernel writes at most q.Size bytes, which is
	// len(buf).
	q.Data = uint64(uintptr(unsafe.Pointer(&buf[0]))) // #nosec G103 -- the xe query ABI carries the output buffer address; buf is pinned (SAFETY above)
	if err := xeIoctl(f, &q); err != nil {
		return nil, fmt.Errorf("xe query data: %w", err)
	}
	return buf[:q.Size], nil
}

func xeIoctl(f *os.File, q *xeDeviceQuery) error {
	conn, err := f.SyscallConn()
	if err != nil {
		return err
	}
	const maxAttempts = 8
	var errno unix.Errno
	if err := conn.Control(func(fd uintptr) {
		for range maxAttempts {
			// SAFETY: q points to a live xeDeviceQuery whose layout matches
			// struct drm_xe_device_query (40 bytes); the conversion happens in
			// the Syscall argument list, as unsafe.Pointer rule 4 requires.
			_, _, errno = unix.Syscall(unix.SYS_IOCTL, fd, xeIoctlDeviceQuery, uintptr(unsafe.Pointer(q))) // #nosec G103 -- ioctl argument of the fixed drm_xe_device_query layout (SAFETY above)
			if errno != unix.EINTR && errno != unix.EAGAIN {
				return
			}
		}
	}); err != nil {
		return err
	}
	if errno != 0 {
		return errno
	}
	return nil
}

// readXeSysfs measures the host's xe driven Intel GPUs.
func readXeSysfs() ([]GpuStat, error) {
	stats := readXeDevices("/sys", "/dev", xeQueryMemRegions)
	if len(stats) == 0 {
		return nil, ErrNoGpuTool
	}
	return stats, nil
}
