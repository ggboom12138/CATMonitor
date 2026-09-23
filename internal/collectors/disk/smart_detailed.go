//go:build linux

package disk

import (
	"strings"
	"time"

	"github.com/Computing-Availability-Tools/CATMonitor/internal/collector"
	"github.com/Computing-Availability-Tools/CATMonitor/internal/source/smartctl"
)

// detailedSmartNames are the metrics produced by collectSMARTDetailed. The
// legacy text-based collectSMART path is skipped whenever any of them is
// wanted, because this path supersedes it: it covers RAID passthrough
// channels, emits smart_status/smart_temperature from the normalized JSON,
// and correctly reports failing disks (smartctl encodes "health FAILED" in
// the exit code, which the legacy path negative-caches).
var detailedSmartNames = []string{
	"smart_wear_percent",
	"smart_power_on_hours",
	"smart_power_cycles",
	"smart_data_written_total",
	"smart_data_read_total",
	"smart_media_errors",
	"smart_reallocated_sectors",
	"smart_available_spare",
	"smart_unsafe_shutdowns",
}

// collectSMARTDetailed queries `smartctl -j -a` for every monitored device
// and for every RAID passthrough channel discovered via `smartctl --scan`,
// emitting per-device SMART metrics. Devices without SMART (RAID logical
// volumes) degrade to no metrics. All results come from the source-layer
// cache (60s TTL + negative cache), so the 2s collection cadence does not
// re-spawn smartctl.
func (c *DiskCollector) collectSMARTDetailed(now time.Time) ([]collector.Metric, error) {
	src := smartctl.Default()
	if !src.Available() {
		return nil, nil
	}
	var metrics []collector.Metric
	// Direct block devices from /proc/diskstats (already whole-disk filtered).
	devs, err := c.filteredDiskStats()
	if err != nil {
		return nil, err
	}
	for dev := range devs {
		if hi := src.HealthJSON("/dev/"+dev, ""); hi != nil {
			metrics = append(metrics, smartDetailMetrics(dev, hi, now)...)
		}
	}
	// RAID passthrough channels: scan entries whose -d selector carries a
	// channel number (e.g. "megaraid,0", "cciss,1"). Plain bus types
	// ("scsi", "ata", "nvme") point at devices already covered above.
	for _, entry := range src.Scan() {
		if !strings.Contains(entry.Type, ",") {
			continue
		}
		if hi := src.HealthJSON(entry.Name, entry.Type); hi != nil {
			metrics = append(metrics, smartDetailMetrics(entry.Type, hi, now)...)
		}
	}
	return metrics, nil
}

// smartDetailMetrics maps one HealthInfo to per-device metrics. Fields the
// transport does not provide (the -1 sentinel) are skipped, so a SATA SSD
// simply has no smart_media_errors series and an NVMe SSD no
// smart_reallocated_sectors series.
func smartDetailMetrics(device string, hi *smartctl.HealthInfo, now time.Time) []collector.Metric {
	var metrics []collector.Metric
	add := func(name string, value float64, unit string, extra map[string]string) {
		labels := map[string]string{"device": device}
		for k, v := range extra {
			labels[k] = v
		}
		metrics = append(metrics, collector.Metric{
			Component: "disk", Name: name, Value: value, Unit: unit,
			Labels: labels, Timestamp: now,
		})
	}
	if hi.HasSMART {
		statusStr := "FAILED"
		if hi.Passed {
			statusStr = "PASSED"
		}
		add("smart_status", boolToFloat(hi.Passed), "", map[string]string{"status": statusStr})
	}
	if hi.Temperature >= 0 {
		add("smart_temperature", hi.Temperature, "°C", nil)
	}
	if hi.WearPercent >= 0 {
		add("smart_wear_percent", hi.WearPercent, "%", nil)
	}
	if hi.PowerOnHours >= 0 {
		add("smart_power_on_hours", hi.PowerOnHours, "h", nil)
	}
	if hi.PowerCycles >= 0 {
		add("smart_power_cycles", hi.PowerCycles, "次", nil)
	}
	if hi.WrittenBytes > 0 {
		add("smart_data_written_total", roundFloat(float64(hi.WrittenBytes)/(1024*1024*1024), 2), "GB", nil)
	}
	if hi.ReadBytes > 0 {
		add("smart_data_read_total", roundFloat(float64(hi.ReadBytes)/(1024*1024*1024), 2), "GB", nil)
	}
	if hi.MediaErrors >= 0 {
		add("smart_media_errors", hi.MediaErrors, "次", nil)
	}
	if hi.ReallocatedSectors >= 0 {
		add("smart_reallocated_sectors", hi.ReallocatedSectors, "个", nil)
	}
	if hi.AvailableSpare >= 0 {
		add("smart_available_spare", hi.AvailableSpare, "%", nil)
	}
	if hi.UnsafeShutdowns >= 0 {
		add("smart_unsafe_shutdowns", hi.UnsafeShutdowns, "次", nil)
	}
	return metrics
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
