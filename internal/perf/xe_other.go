//go:build unix && !darwin && !linux

package perf

// readXeSysfs is Linux only: the xe driver memory query is a Linux DRM ioctl.
func readXeSysfs() ([]GpuStat, error) {
	return nil, ErrNotImplemented
}
