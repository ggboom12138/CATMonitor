package main

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"path/filepath"
	"time"

	"github.com/Computing-Availability-Tools/CATMonitor/features/snapshot"
	"github.com/Computing-Availability-Tools/CATMonitor/internal/version"
)

// Handler serves the SSD monitoring API and static SPA. It is a read-only
// consumer: it loads the daemon-produced snapshot.json (session/timestamp/
// refresh) and snapshot_disk.json (per-disk metrics + disk_info specs) from
// Dir on every request and assembles the per-disk views. There is no state:
// throughput/iops/latency are already rates on the daemon side.
type Handler struct {
	dir string
}

// NewHandler creates a Handler that reads snapshots from dir.
func NewHandler(dir string) *Handler {
	return &Handler{dir: dir}
}

// Register mounts the ssd feature at the mux root: the SPA at "/" and
// "/ssd/", the API at "/api/ssd", and static assets at "/ssd/static/"
// (assets are referenced with absolute /ssd/static/... paths). Used by the
// standalone catmonitor-ssd binary.
func Register(mux *http.ServeMux, dir string) {
	h := NewHandler(dir)
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic("ssd: embed sub failed: " + err.Error())
	}
	mux.HandleFunc("/api/ssd", h.handleAPI)
	mux.Handle("/ssd/static/", http.StripPrefix("/ssd/static/", http.FileServer(http.FS(sub))))
	mux.HandleFunc("/ssd/", h.handleIndex)
	mux.HandleFunc("/", h.handleIndex)
}

// readDiskSnapshot loads the global snapshot and the disk component
// snapshot. The daemon writes both atomically (tmp + rename), so a reader
// never observes a partial file.
func (h *Handler) readDiskSnapshot() (*snapshot.GlobalSnapshot, *snapshot.CompSnapshot, error) {
	g, err := snapshot.ReadGlobal(filepath.Join(h.dir, "snapshot.json"))
	if err != nil {
		return nil, nil, err
	}
	c, err := snapshot.ReadComp(filepath.Join(h.dir, "snapshot_disk.json"))
	if err != nil {
		return nil, nil, err
	}
	return g, c, nil
}

// handleAPI returns the per-disk SSD views as SSDResponse JSON.
func (h *Handler) handleAPI(w http.ResponseWriter, r *http.Request) {
	g, c, err := h.readDiskSnapshot()
	if err != nil {
		http.Error(w, `{"error":"snapshot not ready"}`, http.StatusServiceUnavailable)
		return
	}
	disks, overview := buildDiskViews(c.Specs, c.Metrics)
	resp := SSDResponse{
		SessionID:         g.SessionID,
		Version:           version.Version,
		Timestamp:         formatTime(c.Timestamp),
		RefreshIntervalMS: g.RefreshInterval,
		Overview:          overview,
		Disks:             disks,
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	json.NewEncoder(w).Encode(resp)
}

// handleIndex serves the SPA shell.
func (h *Handler) handleIndex(w http.ResponseWriter, r *http.Request) {
	data, err := staticFiles.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "index not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

func formatTime(t time.Time) string {
	return t.Format("2006-01-02 15:04:05")
}
