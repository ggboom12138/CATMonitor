// Health extends the smartctl source with structured JSON queries:
// `smartctl --scan -j` for device discovery (including RAID passthrough
// channels) and `smartctl -j -a` for a vendor-neutral SMART snapshot. NVMe
// (health information log) and ATA (attribute table) outputs are normalized
// into one HealthInfo struct; fields the transport does not provide keep the
// -1 sentinel. This file is additive: the legacy text Health/Info paths stay
// untouched for existing consumers.

package smartctl

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"time"
)

// ScanEntry is one device discovered by `smartctl --scan -j`. Devices behind
// a RAID controller appear as passthrough channels, e.g.
// Name "/dev/bus/0" + Type "megaraid,0".
type ScanEntry struct {
	Name     string // full device path, e.g. "/dev/sda", "/dev/bus/0"
	Type     string // smartctl -d selector, e.g. "scsi", "megaraid,0"
	Protocol string // e.g. "SCSI", "ATA", "NVMe"
}

// HealthInfo is the vendor-neutral SMART snapshot of one device parsed from
// `smartctl -j -a`. Numeric fields use -1 as the "not provided by this
// transport" sentinel; byte counters are 0 when absent.
type HealthInfo struct {
	Model    string
	Serial   string
	Firmware string
	// CapacityBytes is the user capacity in bytes (0 when absent).
	CapacityBytes uint64
	// Interface is the normalized transport: "SATA", "NVMe", "SAS".
	Interface string
	// Media is "ssd", "hdd" or "unknown" (rotation rate not reported, e.g.
	// some RAID logical volumes).
	Media string
	// HasSMART reports whether the device reported a SMART health status at
	// all; RAID logical volumes usually do not.
	HasSMART bool
	Passed   bool

	Temperature        float64 // °C
	WearPercent        float64 // 0-100, consumed lifetime
	PowerOnHours       float64
	PowerCycles        float64
	MediaErrors        float64 // NVMe media and data integrity errors
	ReallocatedSectors float64 // ATA Reallocated_Sector_Ct raw
	AvailableSpare     float64 // NVMe available spare, %
	UnsafeShutdowns    float64 // NVMe unsafe shutdowns
	// WrittenBytes / ReadBytes are lifetime host totals (TBW basis); 0 when
	// the transport does not report them.
	WrittenBytes uint64
	ReadBytes    uint64
}

// scanFetcher / jsonFetcher are swappable seams for tests, mirroring the
// legacy fetcher pattern.
type scanFetcher = func() (string, error)
type jsonFetcher = func(devPath, devType string) (string, error)

func realScanFetch() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "smartctl", "--scan", "-j").Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func realJSONFetch(devPath, devType string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
	defer cancel()
	args := []string{"-j", "-a"}
	if devType != "" {
		args = append(args, "-d", devType)
	}
	args = append(args, devPath)
	out, err := exec.CommandContext(ctx, "smartctl", args...).Output()
	if err != nil {
		// smartctl encodes status bits in the exit code (e.g. bit 0 = health
		// FAILED); the JSON payload is still emitted and complete, so a
		// failing disk must not be treated as an exec failure (which would
		// negative-cache it and hide the failure).
		var ee *exec.ExitError
		if errors.As(err, &ee) && json.Valid(out) {
			return string(out), nil
		}
		return "", err
	}
	return string(out), nil
}

// SetScanFetcher swaps the --scan fetcher for testing and clears the cache.
func SetScanFetcher(f scanFetcher) {
	defaultSrc.scanFetch = f
	defaultSrc.scanCache = ""
	defaultSrc.scanCachedAt = time.Time{}
}

// SetJSONFetcher swaps the -j -a fetcher for testing and clears the cache.
func SetJSONFetcher(f jsonFetcher) {
	defaultSrc.jsonFetch = f
	defaultSrc.jsonCache = make(map[string]string)
	defaultSrc.jsonCachedAt = make(map[string]time.Time)
}

func (s *defaultSource) Scan() []ScanEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.scanCachedAt.IsZero() && time.Since(s.scanCachedAt) < s.cacheTTL {
		return parseScan(s.scanCache)
	}
	out, err := s.scanFetch()
	s.scanCachedAt = time.Now()
	if err != nil {
		// Negative cache: avoid re-spawning smartctl every cycle.
		s.scanCache = ""
		return nil
	}
	s.scanCache = out
	return parseScan(out)
}

// HealthJSON runs `smartctl -j -a [-d devType] devPath` (cached per
// devPath+devType for cacheTTL) and returns the normalized snapshot. On exec
// failure the result is negative-cached and nil is returned without error so
// the collector can degrade.
func (s *defaultSource) HealthJSON(devPath, devType string) *HealthInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := devPath + "|" + devType
	if at, ok := s.jsonCachedAt[key]; ok && !at.IsZero() && time.Since(at) < s.cacheTTL {
		return parseHealthJSON(s.jsonCache[key])
	}
	out, err := s.jsonFetch(devPath, devType)
	s.jsonCachedAt[key] = time.Now()
	if err != nil {
		s.jsonCache[key] = ""
		return nil
	}
	s.jsonCache[key] = out
	return parseHealthJSON(out)
}

// scanJSON mirrors the `smartctl --scan -j` document.
type scanJSON struct {
	Devices []struct {
		Name     string `json:"name"`
		Type     string `json:"type"`
		Protocol string `json:"protocol"`
	} `json:"devices"`
}

func parseScan(out string) []ScanEntry {
	var s scanJSON
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		return nil
	}
	entries := make([]ScanEntry, 0, len(s.Devices))
	for _, d := range s.Devices {
		entries = append(entries, ScanEntry{Name: d.Name, Type: d.Type, Protocol: d.Protocol})
	}
	return entries
}

// smartJSON holds the subset of `smartctl -j -a` fields we normalize from.
type smartJSON struct {
	ModelName        string `json:"model_name"`
	Product          string `json:"product"`
	SerialNumber     string `json:"serial_number"`
	FirmwareVersion  string `json:"firmware_version"`
	LogicalBlockSize uint64 `json:"logical_block_size"`
	UserCapacity     struct {
		Bytes uint64 `json:"bytes"`
	} `json:"user_capacity"`
	// RotationRate is nil when the device does not report one (e.g. RAID
	// logical volumes); 0 means "Solid State Device".
	RotationRate *int `json:"rotation_rate"`
	Device       struct {
		Protocol string `json:"protocol"`
	} `json:"device"`
	SmartStatus *struct {
		Passed bool `json:"passed"`
	} `json:"smart_status"`
	Temperature *struct {
		Current float64 `json:"current"`
	} `json:"temperature"`
	PowerOnTime *struct {
		Hours float64 `json:"hours"`
	} `json:"power_on_time"`
	PowerCycleCount *float64                     `json:"power_cycle_count"`
	NVMe            *nvmeHealthLog               `json:"nvme_smart_health_information_log"`
	ATA             *struct {
		Table []ataAttr `json:"table"`
	} `json:"ata_smart_attributes"`
}

type nvmeHealthLog struct {
	PercentageUsed   float64 `json:"percentage_used"`
	AvailableSpare   float64 `json:"available_spare"`
	DataUnitsRead    float64 `json:"data_units_read"`
	DataUnitsWritten float64 `json:"data_units_written"`
	MediaErrors      float64 `json:"media_errors"`
	UnsafeShutdowns  float64 `json:"unsafe_shutdowns"`
	PowerCycles      float64 `json:"power_cycles"`
	PowerOnHours     float64 `json:"power_on_hours"`
}

type ataAttr struct {
	Name  string `json:"name"`
	Value float64 `json:"value"`
	Raw   struct {
		Value float64 `json:"value"`
	} `json:"raw"`
}

func parseHealthJSON(out string) *HealthInfo {
	var j smartJSON
	if err := json.Unmarshal([]byte(out), &j); err != nil {
		return nil
	}
	hi := &HealthInfo{
		Model:              firstNonEmpty(j.ModelName, j.Product),
		Serial:             j.SerialNumber,
		Firmware:           j.FirmwareVersion,
		CapacityBytes:      j.UserCapacity.Bytes,
		Interface:          normalizeInterface(j.Device.Protocol),
		Media:              mediaOf(j.Device.Protocol, j.RotationRate),
		Temperature:        -1,
		WearPercent:        -1,
		PowerOnHours:       -1,
		PowerCycles:        -1,
		MediaErrors:        -1,
		ReallocatedSectors: -1,
		AvailableSpare:     -1,
		UnsafeShutdowns:    -1,
	}
	if j.SmartStatus != nil {
		hi.HasSMART = true
		hi.Passed = j.SmartStatus.Passed
	}
	if j.Temperature != nil {
		hi.Temperature = j.Temperature.Current
	}
	// smartctl normalizes these across transports; prefer the top-level
	// fields and fall back to the protocol-specific logs below.
	if j.PowerOnTime != nil {
		hi.PowerOnHours = j.PowerOnTime.Hours
	}
	if j.PowerCycleCount != nil {
		hi.PowerCycles = *j.PowerCycleCount
	}
	lba := j.LogicalBlockSize
	if lba == 0 {
		lba = 512
	}
	if j.NVMe != nil {
		n := j.NVMe
		hi.WearPercent = clamp(n.PercentageUsed, 0, 100)
		hi.AvailableSpare = n.AvailableSpare
		hi.MediaErrors = n.MediaErrors
		hi.UnsafeShutdowns = n.UnsafeShutdowns
		// One NVMe data unit is 512,000 bytes (1000 x 512B sectors).
		hi.WrittenBytes = uint64(n.DataUnitsWritten * 512000)
		hi.ReadBytes = uint64(n.DataUnitsRead * 512000)
		if hi.PowerOnHours < 0 {
			hi.PowerOnHours = n.PowerOnHours
		}
		if hi.PowerCycles < 0 {
			hi.PowerCycles = n.PowerCycles
		}
	}
	if j.ATA != nil {
		attr := func(name string) (norm, rawVal float64, found bool) {
			for _, a := range j.ATA.Table {
				if a.Name == name {
					return a.Value, a.Raw.Value, true
				}
			}
			return 0, 0, false
		}
		if norm, _, found := attr("Wear_Leveling_Count"); found {
			// ATA wear is tracked by the normalized value counting down from
			// 100 (Samsung/Intel convention).
			hi.WearPercent = clamp(100-norm, 0, 100)
		}
		if _, rawVal, found := attr("Reallocated_Sector_Ct"); found {
			hi.ReallocatedSectors = rawVal
		}
		if _, rawVal, found := attr("Total_LBAs_Written"); found {
			hi.WrittenBytes = uint64(rawVal * float64(lba))
		}
		if _, rawVal, found := attr("Total_LBAs_Read"); found {
			hi.ReadBytes = uint64(rawVal * float64(lba))
		}
		if hi.PowerOnHours < 0 {
			if _, rawVal, found := attr("Power_On_Hours"); found {
				hi.PowerOnHours = rawVal
			}
		}
		if hi.PowerCycles < 0 {
			if _, rawVal, found := attr("Power_Cycle_Count"); found {
				hi.PowerCycles = rawVal
			}
		}
	}
	return hi
}

// normalizeInterface maps smartctl protocols to user-facing transports.
func normalizeInterface(protocol string) string {
	switch strings.ToUpper(protocol) {
	case "ATA":
		return "SATA"
	case "NVME":
		return "NVMe"
	case "SCSI":
		return "SAS"
	case "":
		return ""
	default:
		return strings.ToUpper(protocol)
	}
}

// mediaOf classifies the medium from the transport and rotation rate.
func mediaOf(protocol string, rr *int) string {
	if strings.EqualFold(protocol, "NVMe") {
		return "ssd"
	}
	if rr == nil {
		return "unknown"
	}
	if *rr == 0 {
		return "ssd"
	}
	return "hdd"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
