package main

import (
	"sort"

	"github.com/Computing-Availability-Tools/CATMonitor/internal/collector"
)

// SSDResponse is the /api/ssd payload: session meta + overview + per-disk
// views. Every disk entry is keyed by the device label of the underlying
// metrics, so all three dimensions (SMART / space / IO) are per-disk.
type SSDResponse struct {
	SessionID         string    `json:"session_id"`
	Version           string    `json:"version"`
	Timestamp         string    `json:"timestamp"`
	RefreshIntervalMS int       `json:"refresh_interval_ms"`
	Overview          Overview  `json:"overview"`
	Disks             []DiskView `json:"disks"`
}

// Overview aggregates the SSD fleet headline numbers.
type Overview struct {
	SSDCount        int     `json:"ssd_count"`
	TotalCapacityGB float64 `json:"total_capacity_gb"`
	AvgSpaceUsage   float64 `json:"avg_space_usage"`
	HealthyCount    int     `json:"healthy_count"`
	FailedCount     int     `json:"failed_count"`
	NoSMARTCount    int     `json:"no_smart_count"`
	MaxWearPercent  float64 `json:"max_wear_percent"`
}

// DiskView is one SSD: identity from disk_info specs, plus the three
// per-disk metric dimensions. Missing dimensions stay nil (RAID physical
// disks have no mounts/diskstats; logical volumes have no SMART).
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
	Passed            *float64 `json:"passed,omitempty"` // 1 healthy / 0 failed
	Temperature       *float64 `json:"temperature,omitempty"`
	WearPercent       *float64 `json:"wear_percent,omitempty"`
	PowerOnHours      *float64 `json:"power_on_hours,omitempty"`
	PowerCycles       *float64 `json:"power_cycles,omitempty"`
	DataWrittenGB     *float64 `json:"data_written_gb,omitempty"`
	DataReadGB        *float64 `json:"data_read_gb,omitempty"`
	MediaErrors       *float64 `json:"media_errors,omitempty"`
	ReallocatedSect   *float64 `json:"reallocated_sectors,omitempty"`
	AvailableSpare    *float64 `json:"available_spare,omitempty"`
	UnsafeShutdowns   *float64 `json:"unsafe_shutdowns,omitempty"`
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

// buildDiskViews assembles the per-disk views from the disk component
// snapshot. specs provide the SSD inventory (disk_info rows with
// media=ssd); metrics are grouped by their device label and attached to the
// matching disk. Metrics of non-SSD devices (no matching spec row) are
// ignored.
func buildDiskViews(specs, metrics []collector.Metric) ([]DiskView, Overview) {
	byDev := map[string]*DiskView{}
	var order []string
	for _, s := range specs {
		if s.Name != "disk_info" || s.Component != "disk" {
			continue
		}
		if s.Labels["media"] != "ssd" {
			continue
		}
		dev := s.Labels["device"]
		if dev == "" || byDev[dev] != nil {
			continue
		}
		byDev[dev] = &DiskView{
			Device:     dev,
			Model:      s.Labels["model"],
			Serial:     s.Labels["serial"],
			Firmware:   s.Labels["firmware"],
			Interface:  s.Labels["interface"],
			CapacityGB: s.Value,
		}
		order = append(order, dev)
	}

	for _, m := range metrics {
		dev := m.Labels["device"]
		dv := byDev[dev]
		if dv == nil {
			continue // non-SSD device or system-level metric
		}
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
			attachIO(dv, m)
		}
	}

	sort.Strings(order)
	disks := make([]DiskView, 0, len(order))
	for _, dev := range order {
		disks = append(disks, *byDev[dev])
	}
	return disks, buildOverview(disks)
}

// setSmart lazily creates the SmartView and applies fn to it.
func setSmart(p **SmartView, fn func(*SmartView)) {
	if *p == nil {
		*p = &SmartView{}
	}
	fn(*p)
}

// attachIO routes one IO metric into the per-disk IOView.
func attachIO(dv *DiskView, m collector.Metric) {
	if dv.IO == nil {
		dv.IO = &IOView{}
	}
	switch m.Name {
	case "throughput":
		if m.Labels["direction"] == "read" {
			dv.IO.ReadThroughputMBs = m.Value
		} else {
			dv.IO.WriteThroughputMBs = m.Value
		}
	case "iops":
		if m.Labels["direction"] == "read" {
			dv.IO.ReadIOPS = m.Value
		} else {
			dv.IO.WriteIOPS = m.Value
		}
	case "read_latency":
		dv.IO.ReadLatencyMS = m.Value
	case "write_latency":
		dv.IO.WriteLatencyMS = m.Value
	case "read_sectors_total":
		dv.IO.ReadTotalGB = m.Value * 512 / 1e9
	case "written_sectors_total":
		dv.IO.WriteTotalGB = m.Value * 512 / 1e9
	}
}

// buildOverview folds the disk views into the headline numbers.
func buildOverview(disks []DiskView) Overview {
	var ov Overview
	ov.SSDCount = len(disks)
	usageSum, usageCount := 0.0, 0
	for _, d := range disks {
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
		if d.Space != nil {
			usageSum += d.Space.UsagePercent
			usageCount++
		}
	}
	if usageCount > 0 {
		ov.AvgSpaceUsage = round2(usageSum / float64(usageCount))
	}
	return ov
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}
