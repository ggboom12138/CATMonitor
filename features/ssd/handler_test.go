package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Computing-Availability-Tools/CATMonitor/internal/collector"
)

// writeSnapshot creates a minimal daemon-style snapshot dir with a global
// snapshot and a disk component snapshot.
func writeSnapshot(t *testing.T, dir, specsJSON, metricsJSON string) {
	t.Helper()
	global := `{"session_id":"1690000000","timestamp":"2026-09-23T10:00:00+08:00",` +
		`"refresh_interval_ms":2000,"health":{"score":95,"grade":"A"}}`
	comp := `{"component":"disk","timestamp":"2026-09-23T10:00:00+08:00",` +
		`"specs":[` + specsJSON + `],"metrics":[` + metricsJSON + `]}`
	if err := os.WriteFile(filepath.Join(dir, "snapshot.json"), []byte(global), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "snapshot_disk.json"), []byte(comp), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHandleAPI(t *testing.T) {
	dir := t.TempDir()
	writeSnapshot(t, dir,
		`{"component":"disk","name":"disk_info","value":960.2,"unit":"GB",`+
			`"labels":{"device":"megaraid,0","media":"ssd","model":"SAMSUNG MZ7LH960HAJR-00005","interface":"SATA"},`+
			`"timestamp":"2026-09-23T10:00:00+08:00"}`,
		`{"component":"disk","name":"smart_wear_percent","value":1,"unit":"%",`+
			`"labels":{"device":"megaraid,0"},"timestamp":"2026-09-23T10:00:00+08:00"}`)

	mux := http.NewServeMux()
	Register(mux, dir)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/ssd")
	if err != nil {
		t.Fatalf("GET /api/ssd failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("content-type: got %q", ct)
	}
	var out SSDResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.SessionID != "1690000000" || out.RefreshIntervalMS != 2000 {
		t.Errorf("session meta: %+v", out)
	}
	if out.Overview.SSDCount != 1 || out.Overview.MaxWearPercent != 1 {
		t.Errorf("overview: %+v", out.Overview)
	}
	if len(out.Disks) != 1 || out.Disks[0].Device != "megaraid,0" {
		t.Fatalf("disks: %+v", out.Disks)
	}
	if out.Disks[0].SMART == nil || out.Disks[0].SMART.WearPercent == nil || *out.Disks[0].SMART.WearPercent != 1 {
		t.Errorf("disk smart: %+v", out.Disks[0].SMART)
	}
}

func TestHandleAPISnapshotNotReady(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux, t.TempDir()) // empty dir: no snapshot files
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/ssd")
	if err != nil {
		t.Fatalf("GET /api/ssd failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status: got %d want 503", resp.StatusCode)
	}
}

func TestHandleIndexServesSPA(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux, t.TempDir())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	for _, path := range []string{"/", "/ssd/", "/ssd/anything"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s failed: %v", path, err)
		}
		body := make([]byte, 512)
		n, _ := resp.Body.Read(body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body[:n]), "SSD") {
			t.Errorf("GET %s: status %d, body %q", path, resp.StatusCode, string(body[:n]))
		}
	}
}

func TestStaticAssetsServed(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux, t.TempDir())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/ssd/static/ssd.js")
	if err != nil {
		t.Fatalf("GET static js failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("static js status: got %d want 200", resp.StatusCode)
	}
}

// TestMetricShapeRoundTrip verifies the collector.Metric JSON shape the
// daemon writes matches what buildDiskViews consumes (labels with device).
func TestMetricShapeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	// Marshal a real collector.Metric to JSON exactly like snapshot files do.
	m := collector.Metric{
		Component: "disk", Name: "smart_temperature", Value: 37, Unit: "°C",
		Labels:    map[string]string{"device": "megaraid,0"},
		Timestamp: time.Now(),
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	specs := `{"component":"disk","name":"disk_info","value":100,"unit":"GB",` +
		`"labels":{"device":"megaraid,0","media":"ssd","model":"M"},"timestamp":"2026-09-23T10:00:00+08:00"}`
	writeSnapshot(t, dir, specs, string(data))

	h := NewHandler(dir)
	g, c, err := h.readDiskSnapshot()
	if err != nil {
		t.Fatalf("readDiskSnapshot: %v", err)
	}
	if g.SessionID != "1690000000" {
		t.Errorf("global session: %+v", g)
	}
	disks, ov := buildDiskViews(c.Specs, c.Metrics)
	if len(disks) != 1 {
		t.Fatalf("expected 1 disk, got %d", len(disks))
	}
	if disks[0].SMART == nil || disks[0].SMART.Temperature == nil || *disks[0].SMART.Temperature != 37 {
		t.Errorf("round-tripped smart: %+v", disks[0].SMART)
	}
	if ov.SSDCount != 1 {
		t.Errorf("overview: %+v", ov)
	}
}
