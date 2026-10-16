package perf

import "slices"

// LatestGpu returns a copy of the most recent GPU snapshot, or nil when no
// snapshot was taken yet.
func (m *Monitor) LatestGpu() []GpuStat {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	snaps := m.gpuRing.Slice()
	if len(snaps) == 0 {
		return nil
	}
	return slices.Clone(snaps[len(snaps)-1])
}
