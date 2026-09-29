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

// Handler serves the disk monitoring API and static SPA. It is a read-only
// consumer: it loads the daemon-produced snapshot.json (session/timestamp/
// refresh) and snapshot_disk.json (per-disk metrics + disk_info specs) from
// Dir on every request and assembles the per-disk views. Realtime IO is
// already rate-based on the daemon side; the optional windowSampler adds
// the 1/6/12/24h traffic/IO totals from its in-memory counter rings.
type Handler struct {
	dir     string
	sampler *windowSampler
}

// NewHandler creates a Handler that reads snapshots from dir.
func NewHandler(dir string) *Handler {
	return &Handler{dir: dir}
}

// Register mounts the ssd feature at the mux root: the SPA at "/" and
// "/ssd/", the API at "/api/ssd", and static assets at "/ssd/static/"
// (assets are referenced with absolute /ssd/static/... paths). Used by the
// standalone catmonitor-ssd binary; no window sampler (library/test use).
func Register(mux *http.ServeMux, dir string) {
	registerWith(mux, NewHandler(dir))
}

// RegisterWithSampler is Register plus a running window sampler whose
// 1/6/12/24h stats are attached to every logical volume and direct disk in
// the API response. The caller owns the sampler lifecycle (Run/stop).
func RegisterWithSampler(mux *http.ServeMux, dir string, samp *windowSampler) {
	h := NewHandler(dir)
	h.sampler = samp
	registerWith(mux, h)
}

func registerWith(mux *http.ServeMux, h *Handler) {
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

// handleAPI returns the grouped per-disk views as SSDResponse JSON.
func (h *Handler) handleAPI(w http.ResponseWriter, r *http.Request) {
	g, c, err := h.readDiskSnapshot()
	if err != nil {
		http.Error(w, `{"error":"snapshot not ready"}`, http.StatusServiceUnavailable)
		return
	}
	groups, overview := buildGroups(c.Specs, c.Metrics)
	if h.sampler != nil {
		// Window stats and the 24h hourly buckets live on the curve
		// sources: logical volumes and direct-attach disks (the devices
		// that own diskstats counters).
		for i := range groups {
			g := &groups[i]
			if g.Logical != nil {
				g.Logical.WindowStats = h.sampler.Windows(g.Logical.Device)
				g.Logical.Hourly = h.sampler.Hourly(g.Logical.Device)
			} else if len(g.Members) == 1 && g.Members[0].IO != nil {
				g.Members[0].WindowStats = h.sampler.Windows(g.Members[0].Device)
				g.Members[0].Hourly = h.sampler.Hourly(g.Members[0].Device)
			}
		}
	}
	resp := SSDResponse{
		SessionID:         g.SessionID,
		Version:           version.Version,
		Timestamp:         formatTime(c.Timestamp),
		RefreshIntervalMS: g.RefreshInterval,
		Overview:          overview,
		Groups:            groups,
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
