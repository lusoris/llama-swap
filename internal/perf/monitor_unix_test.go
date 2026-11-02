//go:build unix && !darwin

package perf

import (
	"bytes"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRocmSmiLine_ValidLine_v5(t *testing.T) {
	// statistics from rocm-smi 5.7.0-1 (Ubuntu 24)
	header := "device,GPU ID,Temperature (Sensor edge) (C),Temperature (Sensor junction) (C),Temperature (Sensor memory) (C),Fan speed (level),Fan speed (%),Fan RPM,Average Graphics Package Power (W),GPU use (%),GPU memory use (%),Memory Activity,VRAM Total Memory (B),VRAM Total Used Memory (B),Card series,Card model,Card vendor,Card SKU"
	line := "card0,0x7551,38.0,38.0,39.0,51,20,1068,15.0,3,0,N/A,34359738368,17179869184,0x1002,0x1002,0x1002,APM123456"

	stat := parseRocmSmiLine(header, line)
	require.NotNil(t, stat)

	assert.Equal(t, 0, stat.ID)
	assert.Equal(t, 38, stat.TempC)
	assert.Equal(t, 39, stat.VramTempC)
	assert.Equal(t, 20.0, stat.FanSpeedPct)
	assert.Equal(t, 15.0, stat.PowerDrawW)
	assert.Equal(t, 3.0, stat.GpuUtilPct)
	assert.Equal(t, 17179869184/(1024*1024), stat.MemUsedMB)
	assert.Equal(t, 34359738368/(1024*1024), stat.MemTotalMB)
	assert.Equal(t, 50.0, stat.MemUtilPct)
}

func TestParseRocmSmiLine_ValidLine_v7(t *testing.T) {
	// statistics from rocm-smi 7.1.1-0ubuntu1 (Ubuntu 26)
	header := "device,Device Name,Device ID,Device Rev,Subsystem ID,GUID,Temperature (Sensor edge) (C),Temperature (Sensor junction) (C),Temperature (Sensor memory) (C),Fan speed (level),Fan speed (%),Fan RPM,Average Graphics Package Power (W),GPU use (%),GPU Memory Allocated (VRAM%),GPU Memory Read/Write Activity (%),Memory Activity,Avg. Memory Bandwidth,VRAM Total Memory (B),VRAM Total Used Memory (B),Card Series,Card Model,Card Vendor,Card SKU,Node ID,GFX Version"
	line := "card0,N/A,0x7551,0xc0,0x1234,55555,38.0,38.0,39.0,51,20,1068,15.0,3,88,0,N/A,0,34359738368,17179869184,N/A,0x7551,0x1002,APM123456,1,gfx1201"

	stat := parseRocmSmiLine(header, line)
	require.NotNil(t, stat)

	assert.Equal(t, 0, stat.ID)
	assert.Equal(t, 38, stat.TempC)
	assert.Equal(t, 39, stat.VramTempC)
	assert.Equal(t, 20.0, stat.FanSpeedPct)
	assert.Equal(t, 15.0, stat.PowerDrawW)
	assert.Equal(t, 3.0, stat.GpuUtilPct)
	assert.Equal(t, 17179869184/(1024*1024), stat.MemUsedMB)
	assert.Equal(t, 34359738368/(1024*1024), stat.MemTotalMB)
	assert.Equal(t, 50.0, stat.MemUtilPct)
}

func TestParseRocmSmiLine_InvalidLines(t *testing.T) {
	header := "device,GPU ID,Temperature (Sensor edge) (C),Temperature (Sensor junction) (C),Temperature (Sensor memory) (C),Fan speed (level),Fan speed (%),Fan RPM,Average Graphics Package Power (W),GPU use (%),GPU memory use (%),Memory Activity,VRAM Total Memory (B),VRAM Total Used Memory (B),Card series,Card model,Card vendor,Card SKU"

	tests := []struct {
		name   string
		header string
		line   string
	}{
		{
			name:   "empty header",
			header: "",
			line:   "card0,0x7551,38.0,38.0,39.0,51,20,1068,15.0,0,0,N/A,34359738368,17179869184,0x1002,0x1002,0x1002,APM123456",
		},
		{
			name:   "empty line",
			header: header,
			line:   "",
		},
		{
			name:   "field count mismatch",
			header: header,
			line:   "card0,0x7551,38.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stat := parseRocmSmiLine(tt.header, tt.line)
			assert.Nil(t, stat)
		})
	}
}

func TestApplyRocmSmiCol_ValidAndMalformed(t *testing.T) {
	result := &GpuStat{
		ID:          -1,
		TempC:       10,
		VramTempC:   20,
		FanSpeedPct: 30,
		PowerDrawW:  40,
		GpuUtilPct:  50,
		MemTotalMB:  1000,
		MemUsedMB:   500,
	}
	var meta rocmSmiMeta

	assert.True(t, applyRocmSmiCol("device", "card2", result, &meta))
	assert.Equal(t, 2, result.ID)
	assert.Equal(t, "card2", meta.device)

	assert.True(t, applyRocmSmiCol("Device Name", "Radeon RX 7900", result, &meta))
	assert.Equal(t, "Radeon RX 7900", meta.deviceName)

	assert.True(t, applyRocmSmiCol("GUID", "guid-1234", result, &meta))
	assert.Equal(t, "guid-1234", result.UUID)

	assert.True(t, applyRocmSmiCol("Card Series", "Navi 31", result, &meta))
	assert.Equal(t, "Navi 31", meta.cardSeries)

	assert.True(t, applyRocmSmiCol("GFX Version", "gfx1100", result, &meta))
	assert.Equal(t, "gfx1100", meta.gfxVersion)

	assert.True(t, applyRocmSmiCol("Temperature (Sensor edge) (C)", "55.0", result, &meta))
	assert.Equal(t, 55, result.TempC)

	assert.True(t, applyRocmSmiCol("Temperature (Sensor memory) (C)", "65.0", result, &meta))
	assert.Equal(t, 65, result.VramTempC)

	assert.True(t, applyRocmSmiCol("Fan speed (%)", "45.5", result, &meta))
	assert.Equal(t, 45.5, result.FanSpeedPct)

	assert.True(t, applyRocmSmiCol("Current Socket Graphics Package Power (W)", "150.0", result, &meta))
	assert.Equal(t, 150.0, result.PowerDrawW)

	assert.True(t, applyRocmSmiCol("Average Graphics Package Power (W)", "160.0", result, &meta))
	assert.Equal(t, 160.0, result.PowerDrawW)

	assert.True(t, applyRocmSmiCol("GPU use (%)", "92.0", result, &meta))
	assert.Equal(t, 92.0, result.GpuUtilPct)

	assert.True(t, applyRocmSmiCol("VRAM Total Memory (B)", "34359738368", result, &meta))
	assert.Equal(t, 32768, result.MemTotalMB)

	assert.True(t, applyRocmSmiCol("VRAM Total Used Memory (B)", "17179869184", result, &meta))
	assert.Equal(t, 16384, result.MemUsedMB)

	// Malformed numeric value keeps previous value and logs debug message
	buf := &bytes.Buffer{}
	logger := logmon.NewWriter(buf)
	logger.SetLogLevel(logmon.LevelDebug)

	assert.True(t, applyRocmSmiCol("Temperature (Sensor edge) (C)", "not-a-number", result, &meta, logger))
	assert.Equal(t, 55, result.TempC) // kept previous value

	assert.True(t, applyRocmSmiCol("Temperature (Sensor memory) (C)", "xyz", result, &meta, logger))
	assert.Equal(t, 65, result.VramTempC) // kept previous value

	assert.True(t, applyRocmSmiCol("Fan speed (%)", "invalid", result, &meta, logger))
	assert.Equal(t, 45.5, result.FanSpeedPct) // kept previous value

	assert.True(t, applyRocmSmiCol("Average Graphics Package Power (W)", "corrupt", result, &meta, logger))
	assert.Equal(t, 160.0, result.PowerDrawW) // kept previous value

	assert.True(t, applyRocmSmiCol("GPU use (%)", "bad", result, &meta, logger))
	assert.Equal(t, 92.0, result.GpuUtilPct) // kept previous value

	assert.True(t, applyRocmSmiCol("VRAM Total Memory (B)", "broken", result, &meta, logger))
	assert.Equal(t, 32768, result.MemTotalMB) // kept previous value

	assert.True(t, applyRocmSmiCol("VRAM Total Used Memory (B)", "damaged", result, &meta, logger))
	assert.Equal(t, 16384, result.MemUsedMB) // kept previous value

	logOutput := buf.String()
	assert.Contains(t, logOutput, "Temperature (Sensor edge) (C)")
	assert.Contains(t, logOutput, "not-a-number")
	assert.Contains(t, logOutput, "VRAM Total Memory (B)")
	assert.Contains(t, logOutput, "broken")
}
