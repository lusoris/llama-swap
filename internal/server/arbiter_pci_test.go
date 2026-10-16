package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writePCIDevice lays out one sysfs PCI device directory as Linux does.
func writePCIDevice(t *testing.T, root, addr, vendor, class string) {
	t.Helper()
	dir := filepath.Join(root, addr)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "vendor"), []byte(vendor+"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "class"), []byte(class+"\n"), 0o644))
}

// Every Intel display-class device is listed, whatever its driver; other
// vendors and Intel devices of other classes are not.
func TestServer_ListIntelGPUs(t *testing.T) {
	root := t.TempDir()
	writePCIDevice(t, root, "0000:00:02.0", "0x8086", "0x030000") // iGPU (i915)
	writePCIDevice(t, root, "0000:03:00.0", "0x8086", "0x038000") // dGPU (xe), display class
	writePCIDevice(t, root, "0000:00:1f.3", "0x8086", "0x040300") // Intel audio
	writePCIDevice(t, root, "0000:06:00.0", "0x10de", "0x030000") // NVIDIA
	writePCIDevice(t, root, "0000:7d:00.0", "0x1002", "0x030000") // AMD

	got, err := listIntelGPUs(root)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"0000:00:02.0", "0000:03:00.0"}, got)
}

// A root that cannot be read, or a device whose files cannot, is an error and
// never an empty host: an empty host would let any Intel reading look whole.
func TestServer_ListIntelGPUs_UnreadableIsError(t *testing.T) {
	_, err := listIntelGPUs(filepath.Join(t.TempDir(), "missing"))
	assert.Error(t, err)

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "0000:03:00.0"), 0o755))
	_, err = listIntelGPUs(root)
	assert.Error(t, err, "no vendor file")

	root = t.TempDir()
	writePCIDevice(t, root, "0000:03:00.0", "0x8086", "0x030000")
	require.NoError(t, os.Remove(filepath.Join(root, "0000:03:00.0", "class")))
	_, err = listIntelGPUs(root)
	assert.Error(t, err, "no class file")
}
