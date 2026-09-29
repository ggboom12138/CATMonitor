// Package storcli provides a data source that wraps the Broadcom/LSI MegaRAID
// `storcli` tool to read the AUTHORITATIVE virtual-drive → physical-drive
// mapping from the RAID controller (something neither sysfs nor smartctl
// expose). When storcli is absent (not installed, or a non-LSI controller)
// the source reports unavailable and callers fall back to capacity-based
// inference — this package is a strictly optional enhancement path.
//
// Binary discovery covers both PATH names and the official RPM location
// (which deliberately does NOT add itself to PATH). All commands use the
// JSON mode (trailing "J" argument) for robust parsing.
package storcli

import (
	"context"
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const execTimeout = 15 * time.Second

// VDInfo is one virtual drive (logical volume) as reported by the
// controller: its drive group, RAID level and size in bytes (binary units:
// storcli "TB"/"GB" are actually TiB/GiB).
type VDInfo struct {
	DG        int
	VD        int
	RAIDLevel string // "RAID0", "RAID1", ...
	SizeBytes uint64
}

// PDInfo is one physical drive behind the controller. DID equals the N in
// smartctl's "megaraid,N" passthrough selector.
type PDInfo struct {
	DID    int
	DG     int
	Serial string
	Model  string
	Media  string // "HDD" / "SSD"
}

// Source is the typed interface for the storcli data source.
type Source interface {
	// Available reports whether a storcli binary was discovered.
	Available() bool
	// VDs lists the controller's virtual drives; nil when unavailable or
	// on query failure.
	VDs() []VDInfo
	// PDs lists the controller's physical drives (DID, drive group, serial,
	// media); nil when unavailable or on query failure.
	PDs() []PDInfo
}

// candidatePaths are tried in order with exec.LookPath: PATH names first,
// then the official RPM location (which does not add itself to PATH).
var candidatePaths = []string{
	"storcli64",
	"storcli",
	"/opt/MegaRAID/storcli/storcli64",
	"/opt/MegaRAID/storcli/storcli",
}

// SetCandidates overrides the discovery candidates (tests use this to make
// the source unavailable without touching the host).
func SetCandidates(c []string) { candidatePaths = c }

// ResetCandidates restores the default discovery candidates.
func ResetCandidates() {
	candidatePaths = []string{
		"storcli64", "storcli",
		"/opt/MegaRAID/storcli/storcli64", "/opt/MegaRAID/storcli/storcli",
	}
}

// runner is the swappable exec seam (arguments after the binary path).
type runner = func(args ...string) (string, error)

// SetRunner swaps the exec seam for testing; nil restores the real one.
func SetRunner(r runner) { defaultSrc.run = r }

func realRun(binary string) runner {
	return func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
		defer cancel()
		out, err := exec.CommandContext(ctx, binary, args...).Output()
		if err != nil {
			return "", err
		}
		return string(out), nil
	}
}

type defaultSource struct {
	binary string
	run    runner
}

var defaultSrc = &defaultSource{run: nil}

// Default returns the process-wide source. Discovery happens lazily on
// first use and is cached (hwinfo queries once at daemon startup).
func Default() Source { return defaultSrc }

func discover() string {
	for _, c := range candidatePaths {
		if path, err := exec.LookPath(c); err == nil {
			return path
		}
	}
	return ""
}

func (s *defaultSource) Available() bool {
	if s.run == nil {
		s.binary = discover()
		if s.binary != "" {
			s.run = realRun(s.binary)
		}
	}
	return s.run != nil
}

// storcliEnvelope is the top-level JSON shape of every storcli J response.
type storcliEnvelope struct {
	Controllers []struct {
		Status struct {
			Status string `json:"Status"`
		} `json:"Command Status"`
		Response map[string]json.RawMessage `json:"Response Data"`
	} `json:"Controllers"`
}

func (s *defaultSource) VDs() []VDInfo {
	if !s.Available() {
		return nil
	}
	out, err := s.run("/c0", "/vall", "show", "J")
	if err != nil {
		return nil
	}
	var env storcliEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil || len(env.Controllers) == 0 {
		return nil
	}
	if env.Controllers[0].Status.Status != "Success" {
		return nil
	}
	var vds []struct {
		DGVD string `json:"DG/VD"`
		TYPE string `json:"TYPE"`
		Size string `json:"Size"`
	}
	if err := json.Unmarshal(env.Controllers[0].Response["Virtual Drives"], &vds); err != nil {
		return nil
	}
	var result []VDInfo
	for _, v := range vds {
		dg, vd := parseDGVD(v.DGVD)
		if dg < 0 {
			continue
		}
		result = append(result, VDInfo{
			DG:        dg,
			VD:        vd,
			RAIDLevel: strings.TrimSpace(v.TYPE),
			SizeBytes: parseSize(v.Size),
		})
	}
	return result
}

func (s *defaultSource) PDs() []PDInfo {
	if !s.Available() {
		return nil
	}
	out, err := s.run("/c0", "/eall", "/sall", "show", "all", "J")
	if err != nil {
		return nil
	}
	var env storcliEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil || len(env.Controllers) == 0 {
		return nil
	}
	if env.Controllers[0].Status.Status != "Success" {
		return nil
	}
	resp := env.Controllers[0].Response
	var result []PDInfo
	for key, raw := range resp {
		if !strings.HasPrefix(key, "Drive ") || strings.Contains(key, " - Detailed") {
			continue
		}
		var summary []struct {
			DID   int    `json:"DID"`
			DG    int    `json:"DG"`
			Model string `json:"Model"`
			Med   string `json:"Med"`
		}
		if err := json.Unmarshal(raw, &summary); err != nil || len(summary) == 0 {
			continue
		}
		pd := PDInfo{
			DID:   summary[0].DID,
			DG:    summary[0].DG,
			Model: strings.TrimSpace(summary[0].Model),
			Media: strings.TrimSpace(summary[0].Med),
		}
		// Serial lives in the matching "- Detailed Information" section,
		// under "<drive> Device attributes" → {"SN": ...}.
		var attrs struct {
			SN string `json:"SN"`
		}
		attrKey := key + " - Detailed Information"
		var sections map[string]json.RawMessage
		if raw2, ok := resp[attrKey]; ok && json.Unmarshal(raw2, &sections) == nil {
			if raw3, ok := sections[key+" Device attributes"]; ok {
				_ = json.Unmarshal(raw3, &attrs)
			}
		}
		pd.Serial = strings.TrimSpace(attrs.SN)
		result = append(result, pd)
	}
	return result
}

// parseDGVD splits storcli's "0/0" (DG/VD) into two ints; (-1, -1) on error.
func parseDGVD(s string) (int, int) {
	parts := strings.SplitN(strings.TrimSpace(s), "/", 2)
	if len(parts) != 2 {
		return -1, -1
	}
	dg, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	vd, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil {
		return -1, -1
	}
	return dg, vd
}

// parseSize converts storcli size strings ("1.090 TB", "893.750 GB") to
// bytes. storcli's TB/GB are binary units (TiB/GiB).
func parseSize(s string) uint64 {
	fields := strings.Fields(strings.TrimSpace(s))
	if len(fields) != 2 {
		return 0
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	switch strings.ToUpper(fields[1]) {
	case "TB":
		return uint64(v * (1 << 40))
	case "GB":
		return uint64(v * (1 << 30))
	case "MB":
		return uint64(v * (1 << 20))
	}
	return 0
}
