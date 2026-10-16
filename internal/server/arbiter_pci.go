package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// sysPCIDevices is where Linux lists PCI devices, one directory per address.
const sysPCIDevices = "/sys/bus/pci/devices"

// pciVendorIntel and pciClassDisplay are the sysfs vendor id of Intel and
// the class prefix of display controllers (VGA, 3D and other display).
const (
	pciVendorIntel  = "0x8086"
	pciClassDisplay = "0x03"
)

// listIntelGPUs returns the PCI address of every Intel display-class device
// under root, whatever driver it is bound to: Level Zero counts i915 devices
// as well as xe ones. A root that cannot be read is an error, never an empty
// host.
func listIntelGPUs(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("listing PCI devices: %w", err)
	}
	var addrs []string
	for _, e := range entries {
		dir := filepath.Join(root, e.Name())
		vendor, err := os.ReadFile(filepath.Join(dir, "vendor"))
		if err != nil {
			return nil, fmt.Errorf("reading PCI vendor of %s: %w", e.Name(), err)
		}
		if strings.TrimSpace(string(vendor)) != pciVendorIntel {
			continue
		}
		class, err := os.ReadFile(filepath.Join(dir, "class"))
		if err != nil {
			return nil, fmt.Errorf("reading PCI class of %s: %w", e.Name(), err)
		}
		if strings.HasPrefix(strings.TrimSpace(string(class)), pciClassDisplay) {
			addrs = append(addrs, e.Name())
		}
	}
	return addrs, nil
}
