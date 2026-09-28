package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeCounterSnapshot writes a disk snapshot whose only metrics are the
// cumulative counters for one device.
func writeCounterSnapshot(t *testing.T, dir, device string, readSect, writeSect, readIOS, writeIOS uint64) {
	t.Helper()
	global := `{"session_id":"s","timestamp":"2026-09-23T10:00:00+08:00","refresh_interval_ms":2000}`
	comp := fmt.Sprintf(`{"component":"disk","timestamp":"2026-09-23T10:00:00+08:00","metrics":[`+
		`{"component":"disk","name":"read_sectors_total","value":%d,"labels":{"device":"%s"},"timestamp":"2026-09-23T10:00:00+08:00"},`+
		`{"component":"disk","name":"written_sectors_total","value":%d,"labels":{"device":"%s"},"timestamp":"2026-09-23T10:00:00+08:00"},`+
		`{"component":"disk","name":"read_ios_total","value":%d,"labels":{"device":"%s"},"timestamp":"2026-09-23T10:00:00+08:00"},`+
		`{"component":"disk","name":"write_ios_total","value":%d,"labels":{"device":"%s"},"timestamp":"2026-09-23T10:00:00+08:00"}]}`,
		readSect, device, writeSect, device, readIOS, device, writeIOS, device)
	if err := os.WriteFile(filepath.Join(dir, "snapshot.json"), []byte(global), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "snapshot_disk.json"), []byte(comp), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWindowSamplerDeltas(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 23, 10, 0, 0, 0, time.Local)
	current := base
	s := newWindowSampler(dir)
	s.now = func() time.Time { return current }

	// t0: 100 sectors read, 50 written, 10/5 IOs.
	writeCounterSnapshot(t, dir, "sdb", 100, 50, 10, 5)
	s.sample()

	// +25h: 100 more sectors, 50 more written, 10/5 more IOs (retention 25h
	// keeps both samples).
	current = base.Add(25 * time.Hour)
	writeCounterSnapshot(t, dir, "sdb", 200, 100, 20, 10)
	s.sample()

	ws := s.Windows("sdb")
	if len(ws) != 4 {
		t.Fatalf("expected 4 windows, got %d: %v", len(ws), ws)
	}
	w24 := ws["24h"]
	// delta = 100 sectors × 512B = 51200 bytes = 0.0477 GiB; IOs 10/5.
	if w24.ReadIOS != 10 || w24.WriteIOS != 5 {
		t.Errorf("24h IOs: got %d/%d want 10/5", w24.ReadIOS, w24.WriteIOS)
	}
	if w24.CoveredMinutes != 24*60 {
		t.Errorf("24h coverage: got %d min want %d", w24.CoveredMinutes, 24*60)
	}
	if w24.ReadGB <= 0 || w24.WriteGB <= 0 {
		t.Errorf("24h GB: got %v/%v want >0", w24.ReadGB, w24.WriteGB)
	}
	// 1h/6h/12h windows exceed the sample span → covered is capped at the
	// ring span (25h > all windows → fully covered here since oldest sample
	// is exactly at start edge).
	if ws["1h"].ReadIOS != 10 {
		t.Errorf("1h IOs: got %d want 10 (counters barely changed)", ws["1h"].ReadIOS)
	}
}

func TestWindowSamplerPartialCoverage(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 23, 10, 0, 0, 0, time.Local)
	current := base
	s := newWindowSampler(dir)
	s.now = func() time.Time { return current }

	writeCounterSnapshot(t, dir, "sdb", 0, 0, 0, 0)
	s.sample()

	current = base.Add(2 * time.Hour)
	writeCounterSnapshot(t, dir, "sdb", 7200, 3600, 100, 50)
	s.sample()

	ws := s.Windows("sdb")
	// 1h window: fully covered (ring spans 2h).
	if ws["1h"].CoveredMinutes != 60 {
		t.Errorf("1h coverage: got %d want 60", ws["1h"].CoveredMinutes)
	}
	if ws["1h"].ReadIOS != 100 {
		t.Errorf("1h IOs: got %d want 100", ws["1h"].ReadIOS)
	}
	// 6h window: ring only spans 2h → partial coverage, delta over 2h.
	if ws["6h"].CoveredMinutes != 120 {
		t.Errorf("6h coverage: got %d want 120 (partial)", ws["6h"].CoveredMinutes)
	}
	if ws["6h"].ReadIOS != 100 {
		t.Errorf("6h IOs: got %d want 100 (best effort over covered span)", ws["6h"].ReadIOS)
	}
	if ws["24h"].CoveredMinutes != 120 {
		t.Errorf("24h coverage: got %d want 120 (partial)", ws["24h"].CoveredMinutes)
	}
}

func TestWindowSamplerCounterRegression(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 23, 10, 0, 0, 0, time.Local)
	current := base
	s := newWindowSampler(dir)
	s.now = func() time.Time { return current }

	writeCounterSnapshot(t, dir, "sdb", 1000, 1000, 100, 100)
	s.sample()

	// Host reboot: counters reset to small values.
	current = base.Add(time.Hour)
	writeCounterSnapshot(t, dir, "sdb", 5, 5, 1, 1)
	s.sample()

	ws := s.Windows("sdb")
	// Ring restarted at the reboot point: 1h window delta = 5-5 = 0 sectors
	// (single post-reset sample), no bogus negative/huge values.
	if ws["1h"].ReadIOS != 0 {
		t.Errorf("post-reset 1h IOs: got %d want 0", ws["1h"].ReadIOS)
	}
	if ws["1h"].ReadGB != 0 {
		t.Errorf("post-reset 1h GB: got %v want 0", ws["1h"].ReadGB)
	}

	// And it keeps accumulating after the reset.
	current = base.Add(2 * time.Hour)
	writeCounterSnapshot(t, dir, "sdb", 500, 500, 50, 50)
	s.sample()
	ws = s.Windows("sdb")
	if ws["1h"].ReadIOS != 49 || ws["1h"].ReadGB <= 0 {
		t.Errorf("post-reset accumulation: IOs got %d want 49, GB %v", ws["1h"].ReadIOS, ws["1h"].ReadGB)
	}
}

// writePartialCounterSnapshot writes a snapshot whose device only has the
// sector counters (no IOS counters) — the shape a stale file from an older
// daemon would produce.
func writePartialCounterSnapshot(t *testing.T, dir, device string, readSect, writeSect uint64) {
	t.Helper()
	global := `{"session_id":"s","timestamp":"2026-09-23T10:00:00+08:00","refresh_interval_ms":2000}`
	comp := fmt.Sprintf(`{"component":"disk","timestamp":"2026-09-23T10:00:00+08:00","metrics":[`+
		`{"component":"disk","name":"read_sectors_total","value":%d,"labels":{"device":"%s"},"timestamp":"2026-09-23T10:00:00+08:00"},`+
		`{"component":"disk","name":"written_sectors_total","value":%d,"labels":{"device":"%s"},"timestamp":"2026-09-23T10:00:00+08:00"}]}`,
		readSect, device, writeSect, device)
	if err := os.WriteFile(filepath.Join(dir, "snapshot.json"), []byte(global), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "snapshot_disk.json"), []byte(comp), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestWindowSamplerIncompleteCounters pins the stale-snapshot guard: a
// device whose counters are incomplete (e.g. an old snapshot without the
// IOS metrics) must not produce a sample — otherwise its zero baseline
// poisons the deltas with the since-boot totals.
func TestWindowSamplerIncompleteCounters(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 23, 10, 0, 0, 0, time.Local)
	current := base
	s := newWindowSampler(dir)
	s.now = func() time.Time { return current }

	// Stale snapshot: sectors present, IOS metrics absent.
	writePartialCounterSnapshot(t, dir, "sdb", 100, 100)
	s.sample()
	if ws := s.Windows("sdb"); len(ws) != 0 {
		t.Fatalf("incomplete counters must not produce samples, got %v", ws)
	}

	// Fresh snapshot with all four counters arrives later: baseline starts
	// HERE, not from the false zero.
	current = base.Add(time.Hour)
	writeCounterSnapshot(t, dir, "sdb", 500, 500, 50, 50)
	s.sample()
	current = base.Add(2 * time.Hour)
	writeCounterSnapshot(t, dir, "sdb", 1500, 1500, 150, 150)
	s.sample()

	ws := s.Windows("sdb")
	// Delta over the last hour (samples at +1h and +2h): 1000 sectors,
	// 100 IOS — NOT 150 IOS from a false zero baseline.
	if ws["1h"].ReadIOS != 100 {
		t.Errorf("1h IOs: got %d want 100 (no false baseline)", ws["1h"].ReadIOS)
	}
	if ws["1h"].ReadGB <= 0 {
		t.Errorf("1h GB: got %v want >0", ws["1h"].ReadGB)
	}
}

func TestWindowSamplerNoSamples(t *testing.T) {
	s := newWindowSampler(t.TempDir())
	if ws := s.Windows("unknown"); len(ws) != 0 {
		t.Errorf("unknown device should yield empty map, got %v", ws)
	}
	// Snapshot not ready: sample() must not panic or record anything.
	s.sample()
	if ws := s.Windows("sdb"); len(ws) != 0 {
		t.Errorf("missing snapshot should yield no samples, got %v", ws)
	}
}
