package snapshot

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Computing-Availability-Tools/CATMonitor/internal/collector"
	"github.com/Computing-Availability-Tools/CATMonitor/internal/source/dmidecode"
	"github.com/Computing-Availability-Tools/CATMonitor/internal/source/lspci"
	"github.com/Computing-Availability-Tools/CATMonitor/internal/source/smartctl"
	"github.com/Computing-Availability-Tools/CATMonitor/internal/source/sys"
)

// CollectHWSpecs gathers one-shot hardware identity specs (server model, GPU,
// NPU, disk, NIC) ONCE at web startup. It is deliberately NOT a registered
// periodic collector: these values are static identity, not time-series, so
// running them every collection cycle (and relying on a stash to keep them
// alive) was a layering mistake. The result is stored on the DataCollector and
// surfaced in every snapshot's Specs field.
//
// cpu/memory statics (model_info, module_info, ...) are still emitted by their
// existing periodic collectors and stashed separately — only the cross-component
// identity specs that nothing else emits live here.
func CollectHWSpecs() []collector.Metric {
	return newHWCollector().collect()
}

// hwCollector wraps the source-layer calls + nvidia-smi/npu-smi exec that
// produce static identity metrics. Not a collector.Collector; not registered.
type hwCollector struct {
	smiPath     string // nvidia-smi
	npuSmiPath  string // npu-smi
	nvidiaAvail bool
	nvidiaMock  string
	npuAvail    bool
	npuMock     string
}

func newHWCollector() *hwCollector {
	c := &hwCollector{smiPath: "nvidia-smi", npuSmiPath: "npu-smi"}
	if _, err := exec.LookPath("nvidia-smi"); err == nil {
		c.nvidiaAvail = true
	}
	if _, err := exec.LookPath("npu-smi"); err == nil {
		c.npuAvail = true
	}
	return c
}

func (c *hwCollector) collect() []collector.Metric {
	now := time.Now()
	var metrics []collector.Metric
	if m := c.deviceModel(now); m != nil {
		metrics = append(metrics, *m)
	}
	if m := c.osInfo(now); m != nil {
		metrics = append(metrics, *m)
	}
	metrics = append(metrics, c.gpuInfo(now)...)
	metrics = append(metrics, c.npuInfo(now)...)
	metrics = append(metrics, c.diskInfo(now)...)
	metrics = append(metrics, c.netInfo(now)...)
	return metrics
}

// deviceModel emits the server/device model from SMBIOS type 1.
func (c *hwCollector) deviceModel(now time.Time) *collector.Metric {
	si, err := dmidecode.Default().SystemInfo()
	if err != nil || si == nil {
		return nil
	}
	return &collector.Metric{
		Component: "system", Name: "device_model", Value: 1, Unit: "",
		Labels: map[string]string{
			"manufacturer":  si.Manufacturer,
			"product_name":  si.ProductName,
			"version":       si.Version,
			"serial_number": si.Serial,
		},
		Timestamp: now,
	}
}

// osInfo emits the operating system identity (PRETTY_NAME, version, kernel).
// Linux reads /etc/os-release + `uname -r`; Windows runs `cmd /c ver`.
func (c *hwCollector) osInfo(now time.Time) *collector.Metric {
	labels := map[string]string{}
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "ver").Output(); err == nil {
			if s := strings.TrimSpace(strings.Trim(string(out), "\r\n ")); s != "" {
				labels["pretty_name"] = s
			}
		}
	} else {
		if data, err := os.ReadFile("/etc/os-release"); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				k, v, ok := strings.Cut(line, "=")
				if !ok {
					continue
				}
				v = strings.Trim(v, "\"'")
				switch k {
				case "PRETTY_NAME":
					labels["pretty_name"] = v
				case "VERSION_ID":
					labels["version_id"] = v
				}
			}
		}
		if out, err := exec.Command("uname", "-r").Output(); err == nil {
			if k := strings.TrimSpace(string(out)); k != "" {
				labels["kernel"] = k
			}
		}
	}
	if len(labels) == 0 {
		return nil
	}
	return &collector.Metric{
		Component: "system", Name: "os_info", Value: 1, Unit: "",
		Labels:    labels,
		Timestamp: now,
	}
}

// gpuInfo emits one gpu_info metric per GPU from nvidia-smi.
func (c *hwCollector) gpuInfo(now time.Time) []collector.Metric {
	if !c.nvidiaAvail {
		return nil
	}
	var output string
	if c.nvidiaMock != "" {
		output = c.nvidiaMock
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, c.smiPath,
			"--query-gpu=index,name,uuid,driver_version",
			"--format=csv,noheader,nounits").Output()
		if err != nil {
			return nil
		}
		output = string(out)
	}
	var metrics []collector.Metric
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := parseCSV(line)
		if len(f) < 4 {
			continue
		}
		metrics = append(metrics, collector.Metric{
			Component: "gpu", Name: "gpu_info", Value: parseFloat(f[0]), Unit: "",
			Labels: map[string]string{
				"gpu_id":         f[0],
				"name":           f[1],
				"uuid":           f[2],
				"driver_version": f[3],
			},
			Timestamp: now,
		})
	}
	return metrics
}

// npuInfo emits one npu_info metric per NPU from npu-smi info.
func (c *hwCollector) npuInfo(now time.Time) []collector.Metric {
	if !c.npuAvail {
		return nil
	}
	var output string
	if c.npuMock != "" {
		output = c.npuMock
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, c.npuSmiPath, "info").Output()
		if err != nil {
			return nil
		}
		output = string(out)
	}
	var metrics []collector.Metric
	var dataLines []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if isNPUDataLine(line) {
			dataLines = append(dataLines, line)
		}
	}
	for i := 0; i+1 < len(dataLines); i += 2 {
		id, name, bus := parseNPUStatic(dataLines[i], dataLines[i+1])
		if id == "" {
			continue
		}
		// Skip non-NPU data lines: valid bus_id contains ":" (e.g. "0000:C1:00.0").
		// Other sections of npu-smi output (error codes, card IDs) produce
		// numeric bus IDs without ":" — these are not NPU info lines.
		if !strings.Contains(bus, ":") {
			continue
		}
		metrics = append(metrics, collector.Metric{
			Component: "npu", Name: "npu_info", Value: parseFloat(id), Unit: "",
			Labels: map[string]string{
				"npu_id": id,
				"name":   name,
				"bus_id": bus,
			},
			Timestamp: now,
		})
	}
	return metrics
}

// diskInfo emits one disk_info metric per real block device plus one per
// RAID passthrough channel discovered via `smartctl --scan`. Device list,
// model and capacity come from /sys/block (always available, no root);
// smartctl only enriches serial/firmware/interface when smartmontools is
// present. The `media` label classifies ssd/hdd/unknown and the `kind` label
// distinguishes physical disks from RAID logical volumes (see directKind /
// isRAIDVendor). The value is the disk size in GB so the UI can sum
// capacities.
func (c *hwCollector) diskInfo(now time.Time) []collector.Metric {
	devs, err := sys.Default().BlockDevices()
	if err != nil {
		return nil
	}
	// RAID passthrough channels present => /sys/block devices need vendor
	// screening; otherwise every /sys/block device is a direct physical
	// disk (no RAID card in front of it).
	hasRAID := hasRAIDChannels()
	var metrics []collector.Metric
	for _, bd := range devs {
		labels := map[string]string{
			"device": bd.Name,
			"model":  bd.Model,
			"media":  mediaLabel(bd.Name),
			"kind":   directKind(bd.Name, hasRAID),
		}
		if di, err := smartctl.Default().Info(bd.Name); err == nil && di != nil {
			if di.Serial != "" {
				labels["serial"] = di.Serial
			}
			if di.Interface != "" {
				labels["interface"] = di.Interface
			}
			if di.Firmware != "" {
				labels["firmware"] = di.Firmware
			}
			if labels["model"] == "" && di.Model != "" {
				labels["model"] = di.Model
			}
		}
		metrics = append(metrics, collector.Metric{
			Component: "disk", Name: "disk_info",
			Value: roundFloat(float64(bd.SizeBytes)/1e9, 1), Unit: "GB",
			Labels:    labels,
			Timestamp: now,
		})
	}
	metrics = append(metrics, raidDiskInfo(now)...)
	return metrics
}

// raidVendorPrefixes lists SCSI vendor strings that identify a RAID
// controller: logical volumes present the CONTROLLER as their vendor, while
// direct disks report "ATA" or the disk manufacturer (SAMSUNG, SEAGATE...).
var raidVendorPrefixes = []string{
	"AVAGO", "LSI", "BROADCOM", "DELL", "PERC", "HP", "HPE",
	"LENOVO", "ADAPTEC", "MICROCHIP", "ARECA", "3WARE", "IBM",
}

// isRAIDVendor reports whether a SCSI vendor string belongs to a RAID
// controller family.
func isRAIDVendor(vendor string) bool {
	v := strings.ToUpper(strings.TrimSpace(vendor))
	for _, p := range raidVendorPrefixes {
		if strings.HasPrefix(v, p) {
			return true
		}
	}
	return false
}

// hasRAIDChannels reports whether smartctl --scan found RAID passthrough
// channels (e.g. "megaraid,0"). When none exist, every /sys/block device is
// a direct physical disk; when they exist, /sys/block devices may be logical
// volumes served by the controller.
func hasRAIDChannels() bool {
	src := smartctl.Default()
	if !src.Available() {
		return false
	}
	for _, entry := range src.Scan() {
		if strings.Contains(entry.Type, ",") {
			return true
		}
	}
	return false
}

// directKind classifies a /sys/block device. NVMe namespaces are always
// physical (no RAID card virtualizes them as nvme*). When the machine has no
// RAID channels the device is a direct physical disk. When RAID channels
// exist, a device whose SCSI vendor is a RAID controller family is a logical
// volume; anything else (e.g. a direct SATA SSD next to a RAID card) stays
// physical.
func directKind(dev string, hasRAID bool) string {
	if strings.HasPrefix(dev, "nvme") || !hasRAID {
		return "physical"
	}
	if isRAIDVendor(sys.Default().DeviceVendor(dev)) {
		return "logical"
	}
	return "physical"
}

// raidDiskInfo emits disk_info rows for physical disks behind a RAID
// controller, discovered via `smartctl --scan` passthrough channels (e.g.
// "megaraid,0"). These devices do not exist under /sys/block, so identity
// (model/serial/capacity/media) comes from the smartctl JSON snapshot.
func raidDiskInfo(now time.Time) []collector.Metric {
	src := smartctl.Default()
	if !src.Available() {
		return nil
	}
	var metrics []collector.Metric
	for _, entry := range src.Scan() {
		if !strings.Contains(entry.Type, ",") {
			continue // plain bus entries are /sys/block devices covered above
		}
		hi := src.HealthJSON(entry.Name, entry.Type)
		if hi == nil || hi.CapacityBytes == 0 {
			continue
		}
		labels := map[string]string{
			"device": entry.Type,
			"media":  hi.Media,
			"kind":   "physical",
		}
		if hi.Model != "" {
			labels["model"] = hi.Model
		}
		if hi.Serial != "" {
			labels["serial"] = hi.Serial
		}
		if hi.Firmware != "" {
			labels["firmware"] = hi.Firmware
		}
		if hi.Interface != "" {
			labels["interface"] = hi.Interface
		}
		metrics = append(metrics, collector.Metric{
			Component: "disk", Name: "disk_info",
			Value: roundFloat(float64(hi.CapacityBytes)/1e9, 1), Unit: "GB",
			Labels:    labels,
			Timestamp: now,
		})
	}
	return metrics
}

// mediaLabel classifies a /sys/block device via its queue/rotational flag.
// "unknown" when the file is absent (some RAID logical volumes).
func mediaLabel(dev string) string {
	rot, err := sys.Default().Rotational(dev)
	switch {
	case err != nil:
		return "unknown"
	case rot:
		return "hdd"
	default:
		return "ssd"
	}
}

// netInfo emits one net_info metric per non-loopback interface from /sys/class/net.
// Each metric carries PCI address and lspci device description when available.
func (c *hwCollector) netInfo(now time.Time) []collector.Metric {
	ifaces, err := sys.Default().NetInterfaces()
	if err != nil {
		return nil
	}
	var metrics []collector.Metric
	idx := 0
	for _, iface := range ifaces {
		if sys.IsVirtualInterface(iface) {
			continue
		}
		info, err := sys.Default().NetInterfaceInfo(iface)
		if err != nil || info == nil {
			continue
		}
		labels := map[string]string{
			"interface": iface,
			"mac":       info.MAC,
			"mtu":       strconv.Itoa(info.MTU),
			"speed":     strconv.Itoa(info.Speed),
			"driver":    info.Driver,
		}
		if info.PciAddr != "" {
			labels["pci_addr"] = info.PciAddr
			if desc := lspci.Default().Description(info.PciAddr); desc != "" {
				labels["pci_device"] = desc
			}
		}
		metrics = append(metrics, collector.Metric{
			Component: "network", Name: "net_info", Value: float64(idx), Unit: "",
			Labels:    labels,
			Timestamp: now,
		})
		idx++
	}
	return metrics
}

// ---- parsing helpers (mirror the gpu/npu collectors' parsing) ----

func parseCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

func parseFloat(s string) float64 {
	v, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v
}

func roundFloat(val float64, precision int) float64 {
	mul := 1.0
	for i := 0; i < precision; i++ {
		mul *= 10
	}
	return float64(int64(val*mul+0.5)) / mul
}

func isNPUDataLine(line string) bool {
	if !strings.HasPrefix(line, "| ") || len(line) < 3 {
		return false
	}
	c := line[2]
	return c >= '0' && c <= '9'
}

// parseNPUStatic extracts (npu_id, name, bus_id) from a paired npu-smi data
// line pair. line1's first pipe-segment is "<id> <name>"; line2's second
// segment is the bus id.
func parseNPUStatic(line1, line2 string) (id, name, bus string) {
	seg1 := splitPipe(line1)
	if len(seg1) < 1 {
		return
	}
	toks := strings.Fields(seg1[0])
	if len(toks) >= 1 {
		id = toks[0]
	}
	if len(toks) >= 2 {
		name = toks[1]
	}
	seg2 := splitPipe(line2)
	if len(seg2) >= 2 {
		bus = strings.TrimSpace(seg2[1])
	}
	return
}

func splitPipe(line string) []string {
	parts := strings.Split(line, "|")
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// setNvidiaMock / setNpuMock inject canned subprocess output for tests.
func (c *hwCollector) setNvidiaMock(out string) { c.nvidiaMock = out; c.nvidiaAvail = true }
func (c *hwCollector) setNpuMock(out string)    { c.npuMock = out; c.npuAvail = true }
