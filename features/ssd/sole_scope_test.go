package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Computing-Availability-Tools/CATMonitor/internal/metrics"
)

// writeFile writes a temp catalog yaml and returns its path.
func writeFile(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// ssdRequiredMetrics are the disk metrics the ssd feature consumes on its
// API. Every one of them must be declared in features/ssd/metrics.yaml so
// the feature is self-sufficient when it is the ONLY enabled feature.
var ssdRequiredMetrics = []string{
	// space (per-disk aggregate)
	"device_space_usage", "device_space_detail",
	// realtime IO
	"throughput", "iops", "read_latency", "write_latency",
	"read_sectors_total", "written_sectors_total",
	// SMART basic + detailed
	"smart_status", "smart_temperature", "smart_wear_percent",
	"smart_power_on_hours", "smart_power_cycles",
	"smart_data_written_total", "smart_data_read_total",
	"smart_media_errors", "smart_reallocated_sectors",
	"smart_available_spare", "smart_unsafe_shutdowns",
}

// TestSoleScope: with features=[ssd] as the ONLY feature whitelist
// (web/dfee/health not enabled), every metric the /api/ssd view consumes
// must be declared in features/ssd/metrics.yaml and survive feature-scoped
// filtering; a metric not declared there is out of scope and dropped. This
// is the self-sufficiency guarantee the metrics.yaml must hold.
func TestSoleScope(t *testing.T) {
	write := func(name, body string) string { return writeFile(t, name, body) }
	defaultCat := `components:
  - component: disk
    metrics:
      - {name: space_usage, priority: High}
      - {name: space_detail, priority: Medium}
      - {name: iops, priority: Medium}
      - {name: throughput, priority: Medium}
      - {name: io_wait, priority: Medium}
      - {name: smart_status, priority: Medium}
      - {name: smart_temperature, priority: Low}
      - {name: io_errors, priority: Low}
      - {name: read_latency, priority: Low}
      - {name: write_latency, priority: Low}
      - {name: read_sectors_total, priority: Medium}
      - {name: written_sectors_total, priority: Medium}
      - {name: read_time_total, priority: Medium}
      - {name: write_time_total, priority: Medium}
      - {name: device_space_usage, priority: Medium}
      - {name: device_space_detail, priority: Medium}
      - {name: smart_wear_percent, priority: Medium}
      - {name: smart_power_on_hours, priority: Medium}
      - {name: smart_power_cycles, priority: Medium}
      - {name: smart_data_written_total, priority: Medium}
      - {name: smart_data_read_total, priority: Medium}
      - {name: smart_media_errors, priority: Medium}
      - {name: smart_reallocated_sectors, priority: Medium}
      - {name: smart_available_spare, priority: Medium}
      - {name: smart_unsafe_shutdowns, priority: Medium}
  - component: cpu
    metrics:
      - {name: usage, priority: Medium}
`
	metrics.Init(write("base.yaml", defaultCat))
	// Sole whitelist: the feature's own metrics.yaml shipped in this repo
	// (test cwd = features/ssd).
	ssdP := "metrics.yaml"
	metrics.LoadFeatureOverrides([]string{ssdP})
	metrics.SetCollectionThreshold("medium")
	metrics.SetFeatureScope([]string{ssdP})

	// Every metric the ssd view consumes must be in scope.
	for _, m := range ssdRequiredMetrics {
		if !metrics.IsWanted("disk", m) {
			t.Errorf("disk/%s must be wanted (declared by sole feature ssd)", m)
		}
	}
	// Metrics NOT declared by ssd must be out of scope (dropped): the
	// non-per-disk ones (mount-point usage, system-level io_wait/io_errors).
	for _, m := range []string{"space_usage", "space_detail", "io_wait", "io_errors", "read_time_total", "write_time_total"} {
		if metrics.IsWanted("disk", m) {
			t.Errorf("disk/%s not declared by ssd -> must be out of scope", m)
		}
	}
	if metrics.IsWanted("cpu", "usage") {
		t.Error("cpu/usage not declared by ssd -> must be out of scope")
	}
}
