package main

import (
	"testing"
	"time"

	"github.com/Computing-Availability-Tools/CATMonitor/internal/collector"
)

func mk(component, name string, value float64, labels map[string]string) collector.Metric {
	return collector.Metric{Component: component, Name: name, Value: value, Labels: labels, Timestamp: time.Now()}
}

func deviceLabels(dev string) map[string]string {
	return map[string]string{"device": dev}
}

func withField(labels map[string]string, field string) map[string]string {
	out := make(map[string]string, len(labels)+1)
	for k, v := range labels {
		out[k] = v
	}
	out["field"] = field
	return out
}

func TestBuildDiskViews(t *testing.T) {
	specs := []collector.Metric{
		mk("disk", "disk_info", 960.2, map[string]string{
			"device": "megaraid,0", "media": "ssd", "model": "SAMSUNG MZ7LH960HAJR-00005",
			"serial": "S45NNA0N662029", "interface": "SATA", "firmware": "HXT7404Q",
		}),
		mk("disk", "disk_info", 1200.2, map[string]string{
			"device": "megaraid,4", "media": "hdd", "model": "SEAGATE ST1200MM0009",
		}),
		mk("disk", "disk_info", 1919.3, map[string]string{
			"device": "sdb", "media": "ssd", "model": "MR9440-8i",
		}),
		mk("disk", "disk_info", 100, map[string]string{
			"device": "sda", "media": "unknown", "model": "MR9440-8i",
		}),
	}
	metrics := []collector.Metric{
		// SSD megaraid,0: full ATA SMART + no space/IO (RAID member).
		mk("disk", "smart_status", 1, map[string]string{"device": "megaraid,0", "status": "PASSED"}),
		mk("disk", "smart_temperature", 37, deviceLabels("megaraid,0")),
		mk("disk", "smart_wear_percent", 1, deviceLabels("megaraid,0")),
		mk("disk", "smart_power_on_hours", 28599, deviceLabels("megaraid,0")),
		mk("disk", "smart_data_written_total", 5500.88, deviceLabels("megaraid,0")),
		// HDD megaraid,4: SCSI subset — must be excluded (media != ssd).
		mk("disk", "smart_temperature", 30, deviceLabels("megaraid,4")),
		// LD sdb: space + IO, no SMART.
		mk("disk", "device_space_usage", 61.18, deviceLabels("sdb")),
		mk("disk", "device_space_detail", 1750.5, withField(deviceLabels("sdb"), "total")),
		mk("disk", "device_space_detail", 1070.8, withField(deviceLabels("sdb"), "used")),
		mk("disk", "device_space_detail", 679.7, withField(deviceLabels("sdb"), "available")),
		mk("disk", "throughput", 12.5, map[string]string{"device": "sdb", "direction": "read"}),
		mk("disk", "throughput", 3.2, map[string]string{"device": "sdb", "direction": "write"}),
		mk("disk", "iops", 320, map[string]string{"device": "sdb", "direction": "read"}),
		mk("disk", "iops", 45, map[string]string{"device": "sdb", "direction": "write"}),
		mk("disk", "read_latency", 0.8, deviceLabels("sdb")),
		mk("disk", "write_latency", 1.9, deviceLabels("sdb")),
		mk("disk", "written_sectors_total", 11535690618, deviceLabels("sdb")),
		// Unknown-media sda: present in specs but filtered out; its metrics ignored.
		mk("disk", "smart_temperature", 0, deviceLabels("sda")),
	}

	disks, ov := buildDiskViews(specs, metrics)

	if len(disks) != 2 {
		t.Fatalf("expected 2 SSDs (megaraid,0 + sdb), got %d: %+v", len(disks), disks)
	}
	if disks[0].Device != "megaraid,0" || disks[1].Device != "sdb" {
		t.Errorf("disks should be sorted by device, got %q %q", disks[0].Device, disks[1].Device)
	}

	raid0 := disks[0]
	if raid0.Model != "SAMSUNG MZ7LH960HAJR-00005" || raid0.Serial != "S45NNA0N662029" {
		t.Errorf("megaraid,0 identity: %+v", raid0)
	}
	if raid0.SMART == nil || raid0.SMART.Passed == nil || *raid0.SMART.Passed != 1 {
		t.Errorf("megaraid,0 smart status: %+v", raid0.SMART)
	}
	if raid0.SMART.WearPercent == nil || *raid0.SMART.WearPercent != 1 {
		t.Errorf("megaraid,0 wear: %+v", raid0.SMART)
	}
	if raid0.Space != nil || raid0.IO != nil {
		t.Errorf("megaraid,0 is a RAID member: no space/IO expected, got %+v %+v", raid0.Space, raid0.IO)
	}

	sdb := disks[1]
	if sdb.SMART != nil {
		t.Errorf("sdb (logical volume) should have no SMART view, got %+v", sdb.SMART)
	}
	if sdb.Space == nil || sdb.Space.UsagePercent != 61.18 || sdb.Space.TotalGB != 1750.5 {
		t.Errorf("sdb space: %+v", sdb.Space)
	}
	if sdb.IO == nil || sdb.IO.ReadThroughputMBs != 12.5 || sdb.IO.WriteThroughputMBs != 3.2 {
		t.Errorf("sdb io: %+v", sdb.IO)
	}
	if sdb.IO.ReadIOPS != 320 || sdb.IO.WriteIOPS != 45 {
		t.Errorf("sdb iops: %+v", sdb.IO)
	}
	if want := 11535690618.0 * 512 / 1e9; sdb.IO.WriteTotalGB != want {
		t.Errorf("sdb write total: got %v want %v", sdb.IO.WriteTotalGB, want)
	}

	// Overview: 2 SSDs, one healthy + one without SMART; max wear 1.
	if ov.SSDCount != 2 || ov.HealthyCount != 1 || ov.NoSMARTCount != 1 || ov.FailedCount != 0 {
		t.Errorf("overview counts: %+v", ov)
	}
	if ov.MaxWearPercent != 1 {
		t.Errorf("max wear: got %v want 1", ov.MaxWearPercent)
	}
	if ov.AvgSpaceUsage != 61.18 {
		t.Errorf("avg usage: got %v want 61.18", ov.AvgSpaceUsage)
	}
	if ov.TotalCapacityGB != 960.2+1919.3 {
		t.Errorf("total capacity: got %v", ov.TotalCapacityGB)
	}
}

func TestBuildDiskViewsEmpty(t *testing.T) {
	disks, ov := buildDiskViews(nil, nil)
	if len(disks) != 0 || ov.SSDCount != 0 {
		t.Errorf("empty input should yield empty views, got %+v %+v", disks, ov)
	}
}

func TestBuildDiskViewsFailedDisk(t *testing.T) {
	specs := []collector.Metric{
		mk("disk", "disk_info", 500, map[string]string{"device": "nvme0n1", "media": "ssd", "model": "X"}),
	}
	metrics := []collector.Metric{
		mk("disk", "smart_status", 0, map[string]string{"device": "nvme0n1", "status": "FAILED"}),
		mk("disk", "smart_wear_percent", 99, deviceLabels("nvme0n1")),
	}
	disks, ov := buildDiskViews(specs, metrics)
	if len(disks) != 1 {
		t.Fatalf("expected 1 disk, got %d", len(disks))
	}
	if disks[0].SMART.Passed == nil || *disks[0].SMART.Passed != 0 {
		t.Errorf("failed disk passed flag: %+v", disks[0].SMART)
	}
	if ov.FailedCount != 1 || ov.HealthyCount != 0 || ov.MaxWearPercent != 99 {
		t.Errorf("overview for failed disk: %+v", ov)
	}
}
