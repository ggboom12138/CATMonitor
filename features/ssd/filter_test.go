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

// TestBuildGroupsRAID mirrors the reference machine: an SSD logical volume
// (sdb, 1919.3GB) built from two Samsung physical SSDs (960.2GB each),
// plus an HDD logical volume and HDD physicals that must all be filtered.
func TestBuildGroupsRAID(t *testing.T) {
	specs := []collector.Metric{
		mk("disk", "disk_info", 1919.3, map[string]string{
			"device": "sdb", "media": "ssd", "kind": "logical", "model": "MR9440-8i",
		}),
		mk("disk", "disk_info", 960.2, map[string]string{
			"device": "megaraid,0", "media": "ssd", "kind": "physical", "model": "SAMSUNG MZ7LH960HAJR-00005",
			"serial": "S45NNA0N662029", "interface": "SATA", "firmware": "HXT7404Q",
		}),
		mk("disk", "disk_info", 960.2, map[string]string{
			"device": "megaraid,1", "media": "ssd", "kind": "physical", "model": "SAMSUNG MZ7LH960HAJR-00005",
			"serial": "S45NNA0N662039", "interface": "SATA", "firmware": "HXT7404Q",
		}),
		// HDD side: entirely filtered out.
		mk("disk", "disk_info", 1199.7, map[string]string{
			"device": "sda", "media": "hdd", "kind": "logical", "model": "MR9440-8i",
		}),
		mk("disk", "disk_info", 1200.2, map[string]string{
			"device": "megaraid,4", "media": "hdd", "kind": "physical", "model": "SEAGATE ST1200MM0009",
		}),
	}
	metrics := []collector.Metric{
		// Logical sdb: space + IO.
		mk("disk", "device_space_usage", 61.18, deviceLabels("sdb")),
		mk("disk", "device_space_detail", 1750.5, withField(deviceLabels("sdb"), "total")),
		mk("disk", "device_space_detail", 1070.8, withField(deviceLabels("sdb"), "used")),
		mk("disk", "device_space_detail", 679.7, withField(deviceLabels("sdb"), "available")),
		mk("disk", "throughput", 12.5, map[string]string{"device": "sdb", "direction": "read"}),
		mk("disk", "throughput", 3.2, map[string]string{"device": "sdb", "direction": "write"}),
		mk("disk", "iops", 320, map[string]string{"device": "sdb", "direction": "read"}),
		mk("disk", "iops", 45, map[string]string{"device": "sdb", "direction": "write"}),
		// Physical members: SMART only.
		mk("disk", "smart_status", 1, map[string]string{"device": "megaraid,0", "status": "PASSED"}),
		mk("disk", "smart_temperature", 37, deviceLabels("megaraid,0")),
		mk("disk", "smart_wear_percent", 1, deviceLabels("megaraid,0")),
		mk("disk", "smart_power_on_hours", 28599, deviceLabels("megaraid,0")),
		mk("disk", "smart_data_written_total", 5500.88, deviceLabels("megaraid,0")),
		mk("disk", "smart_status", 1, map[string]string{"device": "megaraid,1", "status": "PASSED"}),
		mk("disk", "smart_wear_percent", 1, deviceLabels("megaraid,1")),
		// HDD metrics: ignored (no matching SSD spec row).
		mk("disk", "smart_temperature", 30, deviceLabels("megaraid,4")),
	}

	groups, ov := buildGroups(specs, metrics)

	if len(groups) != 1 {
		t.Fatalf("expected 1 group (sdb + 2 members), got %d: %+v", len(groups), groups)
	}
	g := groups[0]
	if g.Logical == nil || g.Logical.Device != "sdb" {
		t.Fatalf("group should wrap logical sdb, got %+v", g.Logical)
	}
	if g.Logical.Space == nil || g.Logical.Space.UsagePercent != 61.18 {
		t.Errorf("logical space: %+v", g.Logical.Space)
	}
	if g.Logical.IO == nil || g.Logical.IO.ReadThroughputMBs != 12.5 {
		t.Errorf("logical io: %+v", g.Logical.IO)
	}
	if len(g.Members) != 2 {
		t.Fatalf("capacity inference should map 2 members (960.2×2≈1919.3), got %d", len(g.Members))
	}
	if g.Members[0].Device != "megaraid,0" || g.Members[1].Device != "megaraid,1" {
		t.Errorf("members sorted: got %q %q", g.Members[0].Device, g.Members[1].Device)
	}
	m0 := g.Members[0]
	if m0.SMART == nil || m0.SMART.Passed == nil || *m0.SMART.Passed != 1 {
		t.Errorf("member smart status: %+v", m0.SMART)
	}
	if m0.SMART.WearPercent == nil || *m0.SMART.WearPercent != 1 {
		t.Errorf("member wear: %+v", m0.SMART)
	}
	// RAID members must not carry space/io.
	if m0.Space != nil || m0.IO != nil {
		t.Errorf("RAID member should have no space/io, got %+v %+v", m0.Space, m0.IO)
	}

	// Overview counts physical only.
	if ov.SSDCount != 2 || ov.HealthyCount != 2 || ov.FailedCount != 0 || ov.NoSMARTCount != 0 {
		t.Errorf("overview counts: %+v", ov)
	}
	if ov.TotalCapacityGB != 960.2*2 {
		t.Errorf("total capacity: got %v want %v", ov.TotalCapacityGB, 960.2*2)
	}
	if ov.MaxWearPercent != 1 {
		t.Errorf("max wear: got %v", ov.MaxWearPercent)
	}
	// Usage average over entities WITH filesystems: only sdb here.
	if ov.AvgSpaceUsage != 61.18 {
		t.Errorf("avg usage: got %v want 61.18", ov.AvgSpaceUsage)
	}
}

// TestBuildGroupsDirect covers a direct-attach machine (no RAID card): each
// physical SSD is a standalone group carrying everything (SMART + space +
// IO).
func TestBuildGroupsDirect(t *testing.T) {
	specs := []collector.Metric{
		mk("disk", "disk_info", 476.9, map[string]string{
			"device": "nvme0n1", "media": "ssd", "kind": "physical", "model": "Samsung SSD 970",
			"interface": "NVMe",
		}),
		mk("disk", "disk_info", 476.9, map[string]string{
			"device": "nvme1n1", "media": "ssd", "kind": "physical", "model": "Samsung SSD 970",
		}),
	}
	metrics := []collector.Metric{
		mk("disk", "smart_status", 1, map[string]string{"device": "nvme0n1", "status": "PASSED"}),
		mk("disk", "smart_wear_percent", 6, deviceLabels("nvme0n1")),
		mk("disk", "device_space_usage", 40.5, deviceLabels("nvme0n1")),
		mk("disk", "device_space_detail", 400, withField(deviceLabels("nvme0n1"), "total")),
		mk("disk", "throughput", 100.5, map[string]string{"device": "nvme0n1", "direction": "read"}),
		mk("disk", "smart_status", 0, map[string]string{"device": "nvme1n1", "status": "FAILED"}),
		mk("disk", "smart_wear_percent", 99, deviceLabels("nvme1n1")),
	}

	groups, ov := buildGroups(specs, metrics)

	if len(groups) != 2 {
		t.Fatalf("expected 2 standalone groups, got %d", len(groups))
	}
	for _, g := range groups {
		if g.Logical != nil {
			t.Errorf("direct machine should have no logical wrapper, got %+v", g.Logical)
		}
		if len(g.Members) != 1 {
			t.Fatalf("standalone group should have 1 member, got %d", len(g.Members))
		}
	}
	n0 := groups[0].Members[0]
	if n0.Device != "nvme0n1" {
		t.Errorf("groups sorted: got %+v", groups[0].Members[0])
	}
	if n0.Space == nil || n0.Space.UsagePercent != 40.5 {
		t.Errorf("direct disk space: %+v", n0.Space)
	}
	if n0.IO == nil || n0.IO.ReadThroughputMBs != 100.5 {
		t.Errorf("direct disk io: %+v", n0.IO)
	}
	if ov.SSDCount != 2 || ov.HealthyCount != 1 || ov.FailedCount != 1 {
		t.Errorf("overview: %+v", ov)
	}
	if ov.MaxWearPercent != 99 {
		t.Errorf("max wear: got %v want 99", ov.MaxWearPercent)
	}
}

// TestBuildGroupsAmbiguousMapping verifies the RAID1 ambiguity guard: a
// 960G logical volume with two 960G physical disks — either single disk
// matches the capacity, so the mapping is ambiguous and both sides degrade
// to standalone groups instead of guessing.
func TestBuildGroupsAmbiguousMapping(t *testing.T) {
	specs := []collector.Metric{
		mk("disk", "disk_info", 960.2, map[string]string{
			"device": "sdb", "media": "ssd", "kind": "logical", "model": "MR9440-8i",
		}),
		mk("disk", "disk_info", 960.2, map[string]string{
			"device": "megaraid,0", "media": "ssd", "kind": "physical", "model": "SAMSUNG A",
		}),
		mk("disk", "disk_info", 960.2, map[string]string{
			"device": "megaraid,1", "media": "ssd", "kind": "physical", "model": "SAMSUNG B",
		}),
	}
	metrics := []collector.Metric{
		mk("disk", "device_space_usage", 50, deviceLabels("sdb")),
		mk("disk", "smart_status", 1, map[string]string{"device": "megaraid,0", "status": "PASSED"}),
		mk("disk", "smart_status", 1, map[string]string{"device": "megaraid,1", "status": "PASSED"}),
	}

	groups, ov := buildGroups(specs, metrics)
	if len(groups) != 3 {
		t.Fatalf("ambiguous mapping: expected 3 standalone groups (1 logical + 2 physical), got %d", len(groups))
	}
	for _, g := range groups {
		if len(g.Members) > 1 {
			t.Errorf("no grouping should happen on ambiguity, got %d members", len(g.Members))
		}
		if g.Logical != nil && len(g.Members) != 0 {
			t.Errorf("logical group should have no members on ambiguity")
		}
	}
	if ov.SSDCount != 2 {
		t.Errorf("physical count: got %d want 2", ov.SSDCount)
	}
}

// TestBuildGroupsUnmatchedLogical: a logical volume whose capacity matches
// no physical combination keeps its usage/IO but shows no members.
func TestBuildGroupsUnmatchedLogical(t *testing.T) {
	specs := []collector.Metric{
		mk("disk", "disk_info", 500, map[string]string{
			"device": "sdb", "media": "ssd", "kind": "logical", "model": "X",
		}),
		mk("disk", "disk_info", 300, map[string]string{
			"device": "megaraid,0", "media": "ssd", "kind": "physical", "model": "Y",
		}),
	}
	metrics := []collector.Metric{
		mk("disk", "device_space_usage", 20, deviceLabels("sdb")),
		mk("disk", "smart_status", 1, map[string]string{"device": "megaraid,0", "status": "PASSED"}),
	}
	groups, _ := buildGroups(specs, metrics)
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(groups))
	}
	for _, g := range groups {
		if g.Logical != nil {
			if len(g.Members) != 0 {
				t.Errorf("unmatched logical should have no members, got %d", len(g.Members))
			}
			if g.Logical.Space == nil || g.Logical.Space.UsagePercent != 20 {
				t.Errorf("unmatched logical keeps usage: %+v", g.Logical.Space)
			}
		}
	}
}

func TestBuildGroupsEmpty(t *testing.T) {
	groups, ov := buildGroups(nil, nil)
	if len(groups) != 0 || ov.SSDCount != 0 {
		t.Errorf("empty input should yield empty groups, got %+v %+v", groups, ov)
	}
}

func TestBuildGroupsFailedDisk(t *testing.T) {
	specs := []collector.Metric{
		mk("disk", "disk_info", 500, map[string]string{
			"device": "nvme0n1", "media": "ssd", "kind": "physical", "model": "X",
		}),
	}
	metrics := []collector.Metric{
		mk("disk", "smart_status", 0, map[string]string{"device": "nvme0n1", "status": "FAILED"}),
		mk("disk", "smart_wear_percent", 99, deviceLabels("nvme0n1")),
	}
	groups, ov := buildGroups(specs, metrics)
	if len(groups) != 1 || len(groups[0].Members) != 1 {
		t.Fatalf("expected 1 standalone group, got %+v", groups)
	}
	if groups[0].Members[0].SMART.Passed == nil || *groups[0].Members[0].SMART.Passed != 0 {
		t.Errorf("failed disk passed flag: %+v", groups[0].Members[0].SMART)
	}
	if ov.FailedCount != 1 || ov.HealthyCount != 0 || ov.MaxWearPercent != 99 {
		t.Errorf("overview for failed disk: %+v", ov)
	}
}

// TestMatchMembersTolerance pins the 1% capacity tolerance boundary.
func TestMatchMembersTolerance(t *testing.T) {
	pdByDev := map[string]*DiskView{
		"a": {Device: "a", CapacityGB: 100},
		"b": {Device: "b", CapacityGB: 100.5},
	}
	// 200 vs 100+100.5=200.5 → 0.25% off, within tolerance.
	lv := &LogicalView{Device: "lv", CapacityGB: 200}
	used := map[string]bool{}
	members := matchMembers(lv, pdByDev, used)
	if len(members) != 2 {
		t.Fatalf("within tolerance: expected 2 members, got %v", members)
	}
	// 250 vs 200.5 → 20% off, no match.
	lv2 := &LogicalView{Device: "lv2", CapacityGB: 250}
	used2 := map[string]bool{}
	if m := matchMembers(lv2, pdByDev, used2); m != nil {
		t.Errorf("outside tolerance: expected nil, got %v", m)
	}
	// Members already used are not considered again.
	if m := matchMembers(lv, pdByDev, used); m != nil {
		t.Errorf("already-used physicals should not rematch, got %v", m)
	}
}
