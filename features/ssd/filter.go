package main

import (
	"math"
	"math/bits"
	"sort"

	"github.com/Computing-Availability-Tools/CATMonitor/internal/collector"
)

// capacityTolerance is the relative capacity slack when matching a RAID
// logical volume against the sum of its member physical disks (RAID metadata
// consumes a small slice; measured ~0.06% on MR9440-8i).
const capacityTolerance = 0.01

// SSDResponse is the /api/ssd payload: session meta + overview + disk
// groups. A group is either a RAID logical volume with its member physical
// SSDs nested inside, or a standalone physical disk (direct-attach, no RAID
// card in front). All metrics stay per-device (the device label).
type SSDResponse struct {
	SessionID         string     `json:"session_id"`
	Version           string     `json:"version"`
	Timestamp         string     `json:"timestamp"`
	RefreshIntervalMS int        `json:"refresh_interval_ms"`
	Overview          Overview   `json:"overview"`
	Groups            []DiskGroup `json:"groups"`
}

// Overview aggregates the SSD fleet headline numbers. Counts and capacities
// are over PHYSICAL disks only; the usage average covers every entity that
// actually has a filesystem (logical volumes and direct disks).
type Overview struct {
	SSDCount        int     `json:"ssd_count"`
	TotalCapacityGB float64 `json:"total_capacity_gb"`
	AvgSpaceUsage   float64 `json:"avg_space_usage"`
	HealthyCount    int     `json:"healthy_count"`
	FailedCount     int     `json:"failed_count"`
	NoSMARTCount    int     `json:"no_smart_count"`
	MaxWearPercent  float64 `json:"max_wear_percent"`
}

// DiskGroup is one rendered unit: a logical volume frame (usage + IO curves
// live here) with its member physical SSDs nested inside, or a standalone
// physical disk (direct-attach: everything lives on the member itself).
type DiskGroup struct {
	// Logical is the RAID logical volume wrapper; nil for direct-attach
	// disks and for physical disks whose LD mapping could not be inferred.
	Logical *LogicalView `json:"logical,omitempty"`
	// Members are the physical SSDs. Empty for logical volumes whose member
	// inference failed.
	Members []DiskView `json:"members"`
}

// LogicalView is the per-logical-volume view: identity + the two dimensions
// that only exist at this layer (filesystem usage and kernel IO stats).
type LogicalView struct {
	Device     string     `json:"device"`
	Model      string     `json:"model"`
	CapacityGB float64    `json:"capacity_gb"`
	Space      *SpaceView `json:"space,omitempty"`
	IO         *IOView    `json:"io,omitempty"`
}

// DiskView is one physical SSD: identity + SMART. Space/IO are only set for
// direct-attach disks (no RAID card), where the physical disk is also the
// block device the kernel sees filesystems and IO stats for.
type DiskView struct {
	Device     string     `json:"device"`
	Model      string     `json:"model"`
	Serial     string     `json:"serial,omitempty"`
	Firmware   string     `json:"firmware,omitempty"`
	Interface  string     `json:"interface,omitempty"`
	CapacityGB float64    `json:"capacity_gb"`
	SMART      *SmartView `json:"smart,omitempty"`
	Space      *SpaceView `json:"space,omitempty"`
	IO         *IOView    `json:"io,omitempty"`
}

// SmartView holds the per-disk SMART snapshot. nil pointer fields mean the
// transport does not provide the attribute.
type SmartView struct {
	Passed          *float64 `json:"passed,omitempty"` // 1 healthy / 0 failed
	Temperature     *float64 `json:"temperature,omitempty"`
	WearPercent     *float64 `json:"wear_percent,omitempty"`
	PowerOnHours    *float64 `json:"power_on_hours,omitempty"`
	PowerCycles     *float64 `json:"power_cycles,omitempty"`
	DataWrittenGB   *float64 `json:"data_written_gb,omitempty"`
	DataReadGB      *float64 `json:"data_read_gb,omitempty"`
	MediaErrors     *float64 `json:"media_errors,omitempty"`
	ReallocatedSect *float64 `json:"reallocated_sectors,omitempty"`
	AvailableSpare  *float64 `json:"available_spare,omitempty"`
	UnsafeShutdowns *float64 `json:"unsafe_shutdowns,omitempty"`
}

// SpaceView is the aggregated whole-disk filesystem usage.
type SpaceView struct {
	UsagePercent float64 `json:"usage_percent"`
	TotalGB      float64 `json:"total_gb"`
	UsedGB       float64 `json:"used_gb"`
	AvailGB      float64 `json:"avail_gb"`
}

// IOView is the per-disk realtime IO state plus cumulative counters.
type IOView struct {
	ReadThroughputMBs  float64 `json:"read_throughput_mb_s"`
	WriteThroughputMBs float64 `json:"write_throughput_mb_s"`
	ReadIOPS           float64 `json:"read_iops"`
	WriteIOPS          float64 `json:"write_iops"`
	ReadLatencyMS      float64 `json:"read_latency_ms"`
	WriteLatencyMS     float64 `json:"write_latency_ms"`
	ReadTotalGB        float64 `json:"read_total_gb"`
	WriteTotalGB       float64 `json:"write_total_gb"`
}

// buildGroups assembles the disk groups from the disk component snapshot.
// specs provide the SSD inventory (disk_info rows with media=ssd; kind
// separates physical disks from RAID logical volumes); metrics are attached
// to their device (physicals get SMART, logicals get space/IO; direct
// physicals also get space/IO). Logical volumes are then matched to member
// physicals by capacity inference, and everything left over degrades to
// standalone groups.
func buildGroups(specs, metrics []collector.Metric) ([]DiskGroup, Overview) {
	pdByDev := map[string]*DiskView{}
	lvByDev := map[string]*LogicalView{}
	var pdOrder, lvOrder []string
	for _, s := range specs {
		if s.Name != "disk_info" || s.Component != "disk" {
			continue
		}
		if s.Labels["media"] != "ssd" {
			continue
		}
		dev := s.Labels["device"]
		if dev == "" {
			continue
		}
		if s.Labels["kind"] == "logical" {
			if lvByDev[dev] != nil {
				continue
			}
			lvByDev[dev] = &LogicalView{
				Device:     dev,
				Model:      s.Labels["model"],
				CapacityGB: s.Value,
			}
			lvOrder = append(lvOrder, dev)
		} else {
			if pdByDev[dev] != nil {
				continue
			}
			pdByDev[dev] = &DiskView{
				Device:     dev,
				Model:      s.Labels["model"],
				Serial:     s.Labels["serial"],
				Firmware:   s.Labels["firmware"],
				Interface:  s.Labels["interface"],
				CapacityGB: s.Value,
			}
			pdOrder = append(pdOrder, dev)
		}
	}

	for _, m := range metrics {
		dev := m.Labels["device"]
		if dv := pdByDev[dev]; dv != nil {
			attachPhysicalMetric(dv, m)
		} else if lv := lvByDev[dev]; lv != nil {
			attachLogicalMetric(lv, m)
		}
	}

	// Capacity inference: map each logical volume onto the subset of unused
	// physicals that sums up to its capacity.
	used := map[string]bool{}
	var groups []DiskGroup
	for _, dev := range sortedCopy(lvOrder) {
		lv := lvByDev[dev]
		members := matchMembers(lv, pdByDev, used)
		g := DiskGroup{Logical: lv}
		if members != nil {
			g.Members = members
		}
		groups = append(groups, g)
	}
	// Leftover physicals (direct-attach disks, unmapped RAID members).
	for _, dev := range sortedCopy(pdOrder) {
		if !used[dev] {
			groups = append(groups, DiskGroup{Members: []DiskView{*pdByDev[dev]}})
		}
	}
	sortGroups(groups)

	// Overview: physicals only for counts/capacity/health/wear; the usage
	// average over every entity that has a filesystem.
	var physicals []DiskView
	var usages []float64
	for _, dev := range pdOrder {
		physicals = append(physicals, *pdByDev[dev])
	}
	for _, dev := range lvOrder {
		if sp := lvByDev[dev].Space; sp != nil {
			usages = append(usages, sp.UsagePercent)
		}
	}
	for _, dev := range pdOrder {
		if sp := pdByDev[dev].Space; sp != nil {
			usages = append(usages, sp.UsagePercent)
		}
	}
	return groups, buildOverview(physicals, usages)
}

// attachPhysicalMetric routes one metric into a physical disk view.
func attachPhysicalMetric(dv *DiskView, m collector.Metric) {
	switch m.Name {
	case "smart_status":
		if dv.SMART == nil {
			dv.SMART = &SmartView{}
		}
		v := m.Value
		dv.SMART.Passed = &v
	case "smart_temperature":
		setSmart(&dv.SMART, func(s *SmartView) { s.Temperature = &m.Value })
	case "smart_wear_percent":
		setSmart(&dv.SMART, func(s *SmartView) { s.WearPercent = &m.Value })
	case "smart_power_on_hours":
		setSmart(&dv.SMART, func(s *SmartView) { s.PowerOnHours = &m.Value })
	case "smart_power_cycles":
		setSmart(&dv.SMART, func(s *SmartView) { s.PowerCycles = &m.Value })
	case "smart_data_written_total":
		setSmart(&dv.SMART, func(s *SmartView) { s.DataWrittenGB = &m.Value })
	case "smart_data_read_total":
		setSmart(&dv.SMART, func(s *SmartView) { s.DataReadGB = &m.Value })
	case "smart_media_errors":
		setSmart(&dv.SMART, func(s *SmartView) { s.MediaErrors = &m.Value })
	case "smart_reallocated_sectors":
		setSmart(&dv.SMART, func(s *SmartView) { s.ReallocatedSect = &m.Value })
	case "smart_available_spare":
		setSmart(&dv.SMART, func(s *SmartView) { s.AvailableSpare = &m.Value })
	case "smart_unsafe_shutdowns":
		setSmart(&dv.SMART, func(s *SmartView) { s.UnsafeShutdowns = &m.Value })
	case "device_space_usage":
		if dv.Space == nil {
			dv.Space = &SpaceView{}
		}
		dv.Space.UsagePercent = m.Value
	case "device_space_detail":
		if dv.Space == nil {
			dv.Space = &SpaceView{}
		}
		switch m.Labels["field"] {
		case "total":
			dv.Space.TotalGB = m.Value
		case "used":
			dv.Space.UsedGB = m.Value
		case "available":
			dv.Space.AvailGB = m.Value
		}
	case "throughput", "iops", "read_latency", "write_latency",
		"read_sectors_total", "written_sectors_total":
		attachIO(&dv.IO, m)
	}
}

// attachLogicalMetric routes one metric into a logical volume view (space
// and IO are the two dimensions that only exist at the logical layer).
func attachLogicalMetric(lv *LogicalView, m collector.Metric) {
	switch m.Name {
	case "device_space_usage":
		if lv.Space == nil {
			lv.Space = &SpaceView{}
		}
		lv.Space.UsagePercent = m.Value
	case "device_space_detail":
		if lv.Space == nil {
			lv.Space = &SpaceView{}
		}
		switch m.Labels["field"] {
		case "total":
			lv.Space.TotalGB = m.Value
		case "used":
			lv.Space.UsedGB = m.Value
		case "available":
			lv.Space.AvailGB = m.Value
		}
	case "throughput", "iops", "read_latency", "write_latency",
		"read_sectors_total", "written_sectors_total":
		attachIO(&lv.IO, m)
	}
}

// setSmart lazily creates the SmartView and applies fn to it.
func setSmart(p **SmartView, fn func(*SmartView)) {
	if *p == nil {
		*p = &SmartView{}
	}
	fn(*p)
}

// attachIO routes one IO metric into an IOView.
func attachIO(p **IOView, m collector.Metric) {
	if *p == nil {
		*p = &IOView{}
	}
	io := *p
	switch m.Name {
	case "throughput":
		if m.Labels["direction"] == "read" {
			io.ReadThroughputMBs = m.Value
		} else {
			io.WriteThroughputMBs = m.Value
		}
	case "iops":
		if m.Labels["direction"] == "read" {
			io.ReadIOPS = m.Value
		} else {
			io.WriteIOPS = m.Value
		}
	case "read_latency":
		io.ReadLatencyMS = m.Value
	case "write_latency":
		io.WriteLatencyMS = m.Value
	case "read_sectors_total":
		io.ReadTotalGB = m.Value * 512 / 1e9
	case "written_sectors_total":
		io.WriteTotalGB = m.Value * 512 / 1e9
	}
}

// matchMembers finds the subset of unused physical SSDs whose capacity sum
// matches the logical volume within capacityTolerance. It returns the
// smallest matching subset; multiple equally-small candidates (e.g. a RAID1
// mirror, where either single disk matches the volume) are treated as
// ambiguous and return nil so callers degrade instead of guessing. With more
// than 16 free physicals only single-disk (1:1) matches are considered to
// keep the search bounded.
func matchMembers(lv *LogicalView, pdByDev map[string]*DiskView, used map[string]bool) []DiskView {
	var free []string
	for dev := range pdByDev {
		if !used[dev] {
			free = append(free, dev)
		}
	}
	sort.Strings(free)
	n := len(free)
	if n == 0 || lv.CapacityGB <= 0 {
		return nil
	}
	bestPopcount, candidates, bestMask := -1, 0, 0
	singleOnly := n > 16
	for mask := 1; mask < (1 << n); mask++ {
		if singleOnly && bits.OnesCount(uint(mask)) > 1 {
			continue
		}
		sum := 0.0
		for i := 0; i < n; i++ {
			if mask&(1<<i) != 0 {
				sum += pdByDev[free[i]].CapacityGB
			}
		}
		if math.Abs(sum-lv.CapacityGB)/lv.CapacityGB > capacityTolerance {
			continue
		}
		pc := bits.OnesCount(uint(mask))
		switch {
		case bestPopcount == -1 || pc < bestPopcount:
			bestPopcount, candidates, bestMask = pc, 1, mask
		case pc == bestPopcount:
			candidates++
		}
	}
	if bestPopcount == -1 || candidates != 1 {
		return nil // no match, or ambiguous (e.g. RAID1 mirror)
	}
	var members []DiskView
	for i := 0; i < n; i++ {
		if bestMask&(1<<i) != 0 {
			used[free[i]] = true
			members = append(members, *pdByDev[free[i]])
		}
	}
	return members
}

func sortedCopy(order []string) []string {
	out := append([]string(nil), order...)
	sort.Strings(out)
	return out
}

// sortGroups orders groups deterministically by their display name (the
// logical volume's device, or the first member's device for standalone
// groups).
func sortGroups(groups []DiskGroup) {
	name := func(g DiskGroup) string {
		if g.Logical != nil {
			return g.Logical.Device
		}
		if len(g.Members) > 0 {
			return g.Members[0].Device
		}
		return ""
	}
	sort.SliceStable(groups, func(i, j int) bool { return name(groups[i]) < name(groups[j]) })
}

// buildOverview folds the physical disks (and filesystem usages) into the
// headline numbers.
func buildOverview(physicals []DiskView, usages []float64) Overview {
	var ov Overview
	ov.SSDCount = len(physicals)
	for _, d := range physicals {
		ov.TotalCapacityGB += d.CapacityGB
		if d.SMART != nil && d.SMART.Passed != nil {
			if *d.SMART.Passed == 1 {
				ov.HealthyCount++
			} else {
				ov.FailedCount++
			}
		} else {
			ov.NoSMARTCount++
		}
		if d.SMART != nil && d.SMART.WearPercent != nil && *d.SMART.WearPercent > ov.MaxWearPercent {
			ov.MaxWearPercent = *d.SMART.WearPercent
		}
	}
	if len(usages) > 0 {
		sum := 0.0
		for _, u := range usages {
			sum += u
		}
		ov.AvgSpaceUsage = round2(sum / float64(len(usages)))
	}
	return ov
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}
