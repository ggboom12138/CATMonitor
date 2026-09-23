//go:build linux

package disk

import (
	"os"
	"testing"
	"time"

	"github.com/Computing-Availability-Tools/CATMonitor/internal/source/proc"
	"github.com/Computing-Availability-Tools/CATMonitor/internal/source/smartctl"
	"github.com/Computing-Availability-Tools/CATMonitor/internal/source/statfs"
	"github.com/Computing-Availability-Tools/CATMonitor/internal/source/sys"
)

const (
	testdataSys    = "../../../tests/testdata/sys"
	mountsLVMFixt  = "../../../tests/testdata/proc/mounts-lvm.txt"
	ataJSONFixt    = "../../../tests/testdata/smartctl-ata-json.txt"
	scsiJSONFixt   = "../../../tests/testdata/smartctl-scsi-json.txt"
	scanJSONFixt   = "../../../tests/testdata/smartctl-scan-json.txt"
)

// useSysTestdata redirects the sys source at the fixture tree and the mounts
// file at the LVM scenario.
func useSysTestdata(t *testing.T) {
	t.Helper()
	sys.SetRoot(testdataSys)
	proc.SetMountsPath(mountsLVMFixt)
	t.Cleanup(func() {
		sys.SetRoot("/sys")
		proc.SetMountsPath("")
	})
}

func TestParentDisk(t *testing.T) {
	cases := map[string]string{
		"sdb2":        "sdb",
		"sda12":       "sda",
		"nvme0n1p2":   "nvme0n1",
		"mmcblk0p3":   "mmcblk0",
		"sdb":         "",
		"nvme0n1":     "",
		"loop0":       "loop", // whole-device names ending in digits strip too; callers verify against the disk set
		"unresolved9": "unresolved",
	}
	for in, want := range cases {
		if got := parentDisk(in); got != want {
			t.Errorf("parentDisk(%q): got %q want %q", in, got, want)
		}
	}
}

func TestDeviceResolverResolve(t *testing.T) {
	useSysTestdata(t)
	r := newDeviceResolver()
	cases := map[string]string{
		"/dev/sdb2":                  "sdb",
		"/dev/sda":                   "sda",
		"/dev/mapper/openeuler-root": "sdb", // dm-0 -> sdb3 -> sdb
		"/dev/mapper/openeuler-home": "sdb", // dm-1 -> sdb3 -> sdb
		"/dev/mapper/missing":        "",
		"/dev/nvme0n1p2":             "", // nvme0n1 not present in fixture
		"/dev/md0":                   "",
	}
	for in, want := range cases {
		if got := r.resolve(in); got != want {
			t.Errorf("resolve(%q): got %q want %q", in, got, want)
		}
	}
}

func TestCollectDeviceUsage(t *testing.T) {
	useSysTestdata(t)
	// Known statfs values per mount point (bytes).
	sizes := map[string]statfs.Statfs{
		"/boot": {Total: 1_000_000_000, Used: 400_000_000, Avail: 600_000_000, Free: 600_000_000},
		"/":     {Total: 100_000_000_000, Used: 50_000_000_000, Avail: 50_000_000_000, Free: 50_000_000_000},
		"/home": {Total: 50_000_000_000, Used: 10_000_000_000, Avail: 40_000_000_000, Free: 40_000_000_000},
	}
	statfs.SetFetcher(func(path string) (*statfs.Statfs, error) {
		if st, ok := sizes[path]; ok {
			return &st, nil
		}
		return nil, os.ErrNotExist
	})
	t.Cleanup(statfs.ResetFetcher)

	c := New()
	now := time.Now()
	metrics, err := c.collectDeviceUsage(now)
	if err != nil {
		t.Fatalf("collectDeviceUsage failed: %v", err)
	}
	// All mounts live on sdb (sdb2 + dm-0 + dm-1 both slave to sdb3):
	// 1 usage + 3 detail rows, all with device=sdb.
	if len(metrics) != 4 {
		t.Fatalf("expected 4 metrics, got %d: %+v", len(metrics), metrics)
	}
	for _, m := range metrics {
		if m.Labels["device"] != "sdb" {
			t.Errorf("expected device=sdb, got %q", m.Labels["device"])
		}
		if m.Component != "disk" {
			t.Errorf("expected component disk, got %s", m.Component)
		}
	}
	// usage = (0.4 + 50 + 10) / (1 + 100 + 50) = 60.4/151 = 40%
	var usage float64
	fields := map[string]float64{}
	for _, m := range metrics {
		if m.Name == "device_space_usage" {
			usage = m.Value
		}
		if m.Name == "device_space_detail" {
			fields[m.Labels["field"]] = m.Value
		}
	}
	if usage != 40 {
		t.Errorf("device_space_usage: got %v want 40", usage)
	}
	giB := func(v uint64) float64 { return roundFloat(float64(v)/(1024*1024*1024), 2) }
	if fields["total"] != giB(151_000_000_000) {
		t.Errorf("total: got %v want %v", fields["total"], giB(151_000_000_000))
	}
	if fields["used"] != giB(60_400_000_000) {
		t.Errorf("used: got %v want %v", fields["used"], giB(60_400_000_000))
	}
}

func TestCollectDeviceUsageSkipsVirtualFS(t *testing.T) {
	useSysTestdata(t)
	// statfs would fail on virtual mounts anyway; assert no metrics when all
	// real mounts fail to stat.
	statfs.SetFetcher(func(path string) (*statfs.Statfs, error) {
		return nil, os.ErrNotExist
	})
	t.Cleanup(statfs.ResetFetcher)
	c := New()
	metrics, err := c.collectDeviceUsage(time.Now())
	if err != nil {
		t.Fatalf("collectDeviceUsage failed: %v", err)
	}
	if len(metrics) != 0 {
		t.Errorf("expected 0 metrics when statfs fails everywhere, got %d", len(metrics))
	}
}

func TestCollectSMARTDetailed(t *testing.T) {
	useTestdata(t) // fixture diskstats: sda, sdb
	ata := readFile(t, ataJSONFixt)
	scsi := readFile(t, scsiJSONFixt)
	scan := readFile(t, scanJSONFixt)

	smartctl.SetScanFetcher(func() (string, error) { return scan, nil })
	smartctl.SetJSONFetcher(func(devPath, devType string) (string, error) {
		switch {
		case devType == "megaraid,0":
			return ata, nil
		case devType == "megaraid,4":
			return scsi, nil
		case devPath == "/dev/sda":
			return ata, nil
		default:
			// /dev/sdb is a RAID logical volume without SMART.
			return "", os.ErrPermission
		}
	})

	c := New()
	now := time.Now()
	metrics, err := c.collectSMARTDetailed(now)
	if err != nil {
		t.Fatalf("collectSMARTDetailed failed: %v", err)
	}

	// Index by device for assertions.
	byDev := map[string]map[string]float64{}
	statusByDev := map[string]string{}
	for _, m := range metrics {
		dev := m.Labels["device"]
		if byDev[dev] == nil {
			byDev[dev] = map[string]float64{}
		}
		byDev[dev][m.Name] = m.Value
		if m.Name == "smart_status" {
			statusByDev[dev] = m.Labels["status"]
		}
	}

	// Direct device sda (ATA fixture: Samsung SSD).
	if _, ok := byDev["sda"]; !ok {
		t.Fatal("expected metrics for direct device sda")
	}
	if byDev["sda"]["smart_wear_percent"] != 1 {
		t.Errorf("sda wear: got %v want 1", byDev["sda"]["smart_wear_percent"])
	}
	if byDev["sda"]["smart_power_on_hours"] != 28599 {
		t.Errorf("sda power_on_hours: got %v", byDev["sda"]["smart_power_on_hours"])
	}
	if statusByDev["sda"] != "PASSED" {
		t.Errorf("sda status: got %q", statusByDev["sda"])
	}
	// ATA-only: no media_errors series.
	if _, ok := byDev["sda"]["smart_media_errors"]; ok {
		t.Error("sda should not have smart_media_errors (ATA transport)")
	}

	// RAID channel megaraid,0 (same ATA fixture).
	if _, ok := byDev["megaraid,0"]; !ok {
		t.Fatal("expected metrics for RAID channel megaraid,0")
	}
	if byDev["megaraid,0"]["smart_wear_percent"] != 1 {
		t.Errorf("megaraid,0 wear: got %v", byDev["megaraid,0"]["smart_wear_percent"])
	}

	// RAID channel megaraid,4 (SCSI fixture: SAS HDD).
	if _, ok := byDev["megaraid,4"]; !ok {
		t.Fatal("expected metrics for RAID channel megaraid,4")
	}
	if byDev["megaraid,4"]["smart_temperature"] != 31 {
		t.Errorf("megaraid,4 temperature: got %v want 31", byDev["megaraid,4"]["smart_temperature"])
	}
	if _, ok := byDev["megaraid,4"]["smart_wear_percent"]; ok {
		t.Error("megaraid,4 (SCSI) should not have smart_wear_percent")
	}

	// sdb (logical volume, no SMART) must be absent.
	if _, ok := byDev["sdb"]; ok {
		t.Error("sdb (RAID logical volume) should have no SMART metrics")
	}

	// Channels 1 and 5 are not mocked (fetcher returns error) and must be
	// negative-cached without emitting anything.
	for _, dev := range []string{"megaraid,1", "megaraid,5"} {
		if _, ok := byDev[dev]; ok {
			t.Errorf("%s should have no metrics (fetch fails)", dev)
		}
	}

	// Every metric carries a device label (per-disk contract).
	for _, m := range metrics {
		if m.Labels["device"] == "" {
			t.Errorf("metric %s missing device label", m.Name)
		}
	}
}

func TestCollectSMARTDetailedUnavailable(t *testing.T) {
	useTestdata(t)
	// Scan fails -> no RAID channels; JSON fails -> no direct metrics.
	smartctl.SetScanFetcher(func() (string, error) { return "", os.ErrPermission })
	smartctl.SetJSONFetcher(func(devPath, devType string) (string, error) { return "", os.ErrPermission })
	c := New()
	metrics, err := c.collectSMARTDetailed(time.Now())
	if err != nil {
		t.Fatalf("collectSMARTDetailed failed: %v", err)
	}
	if len(metrics) != 0 {
		t.Errorf("expected 0 metrics when smartctl fails, got %d", len(metrics))
	}
}
