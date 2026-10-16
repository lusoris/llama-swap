//go:build unix && !darwin

package perf

import (
	"context"
	"time"

	"github.com/mostlygeek/llama-swap/internal/logmon"
)

// trySysfs reports GPUs measured through kernel driver interfaces. It fails
// when no device can be read at start, so the monitor reports no GPU rather
// than an empty one.
func trySysfs(ctx context.Context, every time.Duration, logger *logmon.Monitor) (chan []GpuStat, error) {
	if _, err := readSysfs(); err != nil {
		return nil, err
	}
	if every < time.Second {
		every = time.Second
	}

	ch := make(chan []GpuStat, 1)

	go func() {
		defer close(ch)
		ticker := time.NewTicker(every)
		defer ticker.Stop()

		for ctx.Err() == nil {
			select {
			case <-ctx.Done():
			case <-ticker.C:
				stats, err := readSysfs()
				if err != nil {
					logger.Debugf("sysfs: %s", err.Error())
					continue
				}
				select {
				case ch <- stats:
				default:
				}
			}
		}
	}()

	return ch, nil
}

// readSysfs measures GPUs through kernel driver interfaces. Today that covers
// Intel GPUs on the xe driver; a device it cannot read is omitted.
func readSysfs() ([]GpuStat, error) {
	return readXeSysfs()
}
