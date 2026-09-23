//go:build linux

package disk

import (
	"strings"
	"time"

	"github.com/Computing-Availability-Tools/CATMonitor/internal/collector"
	"github.com/Computing-Availability-Tools/CATMonitor/internal/source/proc"
	"github.com/Computing-Availability-Tools/CATMonitor/internal/source/statfs"
	"github.com/Computing-Availability-Tools/CATMonitor/internal/source/sys"
)

// deviceResolver maps mount source devices (partitions, LVM logical volumes)
// to their parent whole-disk name so per-disk usage can be aggregated.
type deviceResolver struct {
	disks   map[string]bool // whole-disk names: sda, nvme0n1, ...
	dmNames map[string]string // device-mapper name -> dm-N, e.g. "openeuler-root" -> "dm-0"
}

func newDeviceResolver() *deviceResolver {
	r := &deviceResolver{
		disks:   map[string]bool{},
		dmNames: map[string]string{},
	}
	src := sys.Default()
	if devs, err := src.BlockDevices(); err == nil {
		for _, d := range devs {
			r.disks[d.Name] = true
		}
	}
	if names, err := src.BlockNames(); err == nil {
		for _, n := range names {
			if strings.HasPrefix(n, "dm-") {
				if name := src.DMName(n); name != "" {
					r.dmNames[name] = n
				}
			}
		}
	}
	return r
}

// resolve maps a mount device path to the parent whole-disk name:
// "/dev/sdb2" -> "sdb", "/dev/mapper/openeuler-root" -> "dm-0" -> slave
// "sdb3" -> "sdb", "/dev/nvme0n1p2" -> "nvme0n1". Returns "" when the chain
// cannot be resolved to exactly one monitored disk (unknown device, dm chain
// too deep, or an LV striped across several disks).
func (r *deviceResolver) resolve(devPath string) string {
	name := strings.TrimPrefix(devPath, "/dev/")
	if strings.HasPrefix(name, "mapper/") {
		dm, ok := r.dmNames[strings.TrimPrefix(name, "mapper/")]
		if !ok {
			return ""
		}
		name = dm
	}
	// Follow device-mapper chains down to real disk partitions.
	for depth := 0; strings.HasPrefix(name, "dm-") && depth < 8; depth++ {
		slaves, err := sys.Default().DMSlaves(name)
		if err != nil || len(slaves) == 0 {
			return ""
		}
		name = slaves[0]
	}
	if r.disks[name] {
		return name
	}
	if parent := parentDisk(name); parent != "" && r.disks[parent] {
		return parent
	}
	return ""
}

// parentDisk strips a partition suffix: "sdb2" -> "sdb", "nvme0n1p2" ->
// "nvme0n1", "mmcblk0p3" -> "mmcblk0". Returns "" when no partition suffix
// is present.
func parentDisk(part string) string {
	if strings.HasPrefix(part, "nvme") || strings.HasPrefix(part, "mmcblk") {
		if i := strings.LastIndex(part, "p"); i > 0 && isDigits(part[i+1:]) {
			return part[:i]
		}
		return ""
	}
	i := len(part) - 1
	for i >= 0 && part[i] >= '0' && part[i] <= '9' {
		i--
	}
	if i == len(part)-1 {
		return "" // no trailing digits
	}
	return part[:i+1]
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// usageAgg accumulates per-disk space across all filesystems that live on
// the disk (partitions and LVM volumes alike).
type usageAgg struct {
	total uint64
	used  uint64
	avail uint64
}

// collectDeviceUsage aggregates per-mount filesystem usage into per-disk
// metrics: device_space_usage (%) and device_space_detail (GB,
// field=total/used/available). Virtual filesystems are skipped; mounts that
// cannot be attributed to exactly one monitored disk are ignored.
func (c *DiskCollector) collectDeviceUsage(now time.Time) ([]collector.Metric, error) {
	mounts, err := proc.Default().Mounts()
	if err != nil {
		return nil, err
	}
	resolver := newDeviceResolver()
	perDisk := map[string]*usageAgg{}
	seen := map[string]bool{}
	for _, m := range mounts {
		if virtualFS[m.Fstype] || seen[m.Device] {
			continue
		}
		seen[m.Device] = true
		disk := resolver.resolve(m.Device)
		if disk == "" {
			continue
		}
		st, err := statfs.Default().Statfs(m.MountPoint)
		if err != nil {
			continue
		}
		agg := perDisk[disk]
		if agg == nil {
			agg = &usageAgg{}
			perDisk[disk] = agg
		}
		agg.total += st.Total
		agg.used += st.Used
		agg.avail += st.Avail
	}
	var metrics []collector.Metric
	for disk, agg := range perDisk {
		usage := 0.0
		if agg.total > 0 {
			usage = float64(agg.used) / float64(agg.total) * 100
		}
		gb := func(v uint64) float64 { return roundFloat(float64(v)/(1024*1024*1024), 2) }
		labels := map[string]string{"device": disk}
		metrics = append(metrics,
			collector.Metric{Component: "disk", Name: "device_space_usage", Value: roundFloat(usage, 2), Unit: "%", Labels: labels, Timestamp: now},
			collector.Metric{Component: "disk", Name: "device_space_detail", Value: gb(agg.total), Unit: "GB", Labels: withField(labels, "total"), Timestamp: now},
			collector.Metric{Component: "disk", Name: "device_space_detail", Value: gb(agg.used), Unit: "GB", Labels: withField(labels, "used"), Timestamp: now},
			collector.Metric{Component: "disk", Name: "device_space_detail", Value: gb(agg.avail), Unit: "GB", Labels: withField(labels, "available"), Timestamp: now},
		)
	}
	return metrics, nil
}
