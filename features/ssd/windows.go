package main

import (
	"sync"
	"time"
)

// windowKinds lists the reporting windows: label + duration.
var windowKinds = []struct {
	Label string
	Dur   time.Duration
}{
	{"1h", time.Hour},
	{"6h", 6 * time.Hour},
	{"12h", 12 * time.Hour},
	{"24h", 24 * time.Hour},
}

// WindowStat is the traffic/IO delta over one window, computed from the
// kernel's cumulative counters (sectors ×512 / completed IOs).
type WindowStat struct {
	ReadGB         float64 `json:"read_gb"`
	WriteGB        float64 `json:"write_gb"`
	ReadIOS        uint64  `json:"read_ios"`
	WriteIOS       uint64  `json:"write_ios"`
	CoveredMinutes int     `json:"covered_minutes"` // span the delta covers; < window while accumulating
}

// HourBucket is one clock-aligned hour of traffic/IO (e.g. Hour "10:00"
// covers 10:00–11:00). Partial marks the in-progress current hour; buckets
// before the ring's coverage are omitted entirely.
type HourBucket struct {
	Hour     string  `json:"hour"` // bucket start, "15:04"
	ReadGB   float64 `json:"read_gb"`
	WriteGB  float64 `json:"write_gb"`
	ReadIOS  uint64  `json:"read_ios"`
	WriteIOS uint64  `json:"write_ios"`
	Partial  bool    `json:"partial,omitempty"`
}

// windowSample is one point of a per-device counter ring.
type windowSample struct {
	t                            time.Time
	readSect, writeSect          uint64
	readIOS, writeIOS            uint64
}

// counterSet mirrors the snapshot metrics one device contributes. seen
// tracks which counters were present: a snapshot missing any of the four
// (e.g., a stale file from an older daemon without read_ios_total) must not
// produce a false zero baseline.
type counterSet struct {
	readSect, writeSect, readIOS, writeIOS uint64
	seen                                   uint8 // 1|2|4|8 bits
}

const countersComplete = 1 | 2 | 4 | 8

// windowSampler periodically reads the disk component snapshot and keeps
// per-device counter rings (30s cadence, 25h retention) so API requests can
// compute 1/6/12/24h deltas without any persistence: history accumulates
// from process start (in-memory only). Counters that go backwards (host
// reboot resets /proc/diskstats) reset the affected ring.
type windowSampler struct {
	mu      sync.Mutex
	samples map[string][]windowSample
	dir     string
	tick    time.Duration
	now     func() time.Time
}

func newWindowSampler(dir string) *windowSampler {
	return &windowSampler{
		samples: map[string][]windowSample{},
		dir:     dir,
		tick:    30 * time.Second,
		now:     time.Now,
	}
}

// Run blocks, sampling immediately and then once per tick, until stop is
// closed. Started as a goroutine by the standalone binary.
func (s *windowSampler) Run(stop <-chan struct{}) {
	t := time.NewTicker(s.tick)
	defer t.Stop()
	s.sample()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			s.sample()
		}
	}
}

// sample reads the disk snapshot and appends one counter point per device
// that has cumulative counters. Snapshot errors keep the history untouched.
func (s *windowSampler) sample() {
	h := NewHandler(s.dir)
	_, c, err := h.readDiskSnapshot()
	if err != nil {
		return
	}
	now := s.now()
	counters := map[string]*counterSet{}
	for _, m := range c.Metrics {
		dev := m.Labels["device"]
		if dev == "" {
			continue
		}
		cs := counters[dev]
		if cs == nil {
			cs = &counterSet{}
			counters[dev] = cs
		}
		switch m.Name {
		case "read_sectors_total":
			cs.readSect = uint64(m.Value)
			cs.seen |= 1
		case "written_sectors_total":
			cs.writeSect = uint64(m.Value)
			cs.seen |= 2
		case "read_ios_total":
			cs.readIOS = uint64(m.Value)
			cs.seen |= 4
		case "write_ios_total":
			cs.writeIOS = uint64(m.Value)
			cs.seen |= 8
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for dev, cs := range counters {
		if cs.seen != countersComplete {
			continue // incomplete counters (stale snapshot): no sample this round
		}
		ring := s.samples[dev]
		if n := len(ring); n > 0 {
			last := ring[n-1]
			// Counter regression (host reboot): restart the ring so deltas
			// stay monotonic.
			if cs.readSect < last.readSect || cs.writeSect < last.writeSect ||
				cs.readIOS < last.readIOS || cs.writeIOS < last.writeIOS {
				ring = nil
			}
		}
		ring = append(ring, windowSample{
			t: now, readSect: cs.readSect, writeSect: cs.writeSect,
			readIOS: cs.readIOS, writeIOS: cs.writeIOS,
		})
		cutoff := now.Add(-25 * time.Hour)
		idx := 0
		for idx < len(ring) && ring[idx].t.Before(cutoff) {
			idx++
		}
		if idx > 0 {
			ring = ring[idx:]
		}
		s.samples[dev] = ring
	}
}

// Windows returns the 1/6/12/24h stats of one device, keyed by window label.
// An empty map means "no samples yet". While the ring is younger than a
// window, the delta covers the available span and CoveredMinutes reports it.
func (s *windowSampler) Windows(device string) map[string]WindowStat {
	s.mu.Lock()
	defer s.mu.Unlock()
	ring := s.samples[device]
	out := map[string]WindowStat{}
	if len(ring) == 0 {
		return out
	}
	latest := ring[len(ring)-1]
	now := s.now()
	for _, w := range windowKinds {
		start := now.Add(-w.Dur)
		base := ring[0] // ring younger than window: partial from first sample
		for i := len(ring) - 1; i >= 0; i-- {
			if !ring[i].t.After(start) {
				base = ring[i]
				break
			}
		}
		covered := latest.t.Sub(ring[0].t)
		if covered > w.Dur {
			covered = w.Dur
		}
		out[w.Label] = WindowStat{
			ReadGB:         float64(latest.readSect-base.readSect) * 512 / (1024 * 1024 * 1024),
			WriteGB:        float64(latest.writeSect-base.writeSect) * 512 / (1024 * 1024 * 1024),
			ReadIOS:        latest.readIOS - base.readIOS,
			WriteIOS:       latest.writeIOS - base.writeIOS,
			CoveredMinutes: int(covered.Minutes()),
		}
	}
	return out
}

// counterAt returns the newest sample at or before t; nil when the ring
// starts after t.
func (s *windowSampler) counterAt(ring []windowSample, t time.Time) *windowSample {
	for i := len(ring) - 1; i >= 0; i-- {
		if !ring[i].t.After(t) {
			sample := ring[i]
			return &sample
		}
	}
	return nil
}

// Hourly returns up to 24 clock-aligned hour buckets ending at the current
// hour: full hours use the samples at both hour boundaries; the final
// bucket (current hour, in progress) is Partial and runs to the latest
// sample. Buckets whose start boundary predates the ring are omitted, so a
// freshly started sampler yields a short, right-aligned array.
func (s *windowSampler) Hourly(device string) []HourBucket {
	s.mu.Lock()
	defer s.mu.Unlock()
	ring := s.samples[device]
	if len(ring) == 0 {
		return nil
	}
	now := s.now()
	hourStart := now.Truncate(time.Hour)
	const giB = 1024 * 1024 * 1024
	var out []HourBucket
	for i := 23; i >= 0; i-- {
		start := hourStart.Add(-time.Duration(i) * time.Hour)
		var base, next *windowSample
		if i == 0 {
			// Current hour: base at the hour boundary, delta to latest.
			// When the ring starts mid-hour (fresh process), fall back to
			// the oldest sample so the chart shows a partial bar instead
			// of staying blank for up to an hour.
			base = s.counterAt(ring, start)
			next = &ring[len(ring)-1]
			if base == nil {
				base = &ring[0]
			}
			out = append(out, s.bucket(start, base, next, giB, true))
			continue
		}
		end := start.Add(time.Hour)
		base = s.counterAt(ring, start)
		next = s.counterAt(ring, end)
		if base == nil || next == nil {
			continue // boundary not covered by the ring
		}
		out = append(out, s.bucket(start, base, next, giB, false))
	}
	return out
}

// bucket diffs two counter samples into one HourBucket.
func (s *windowSampler) bucket(start time.Time, base, next *windowSample, giB int, partial bool) HourBucket {
	return HourBucket{
		Hour:     start.Format("15:04"),
		ReadGB:   float64(next.readSect-base.readSect) * 512 / float64(giB),
		WriteGB:  float64(next.writeSect-base.writeSect) * 512 / float64(giB),
		ReadIOS:  next.readIOS - base.readIOS,
		WriteIOS: next.writeIOS - base.writeIOS,
		Partial:  partial,
	}
}
