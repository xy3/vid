package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// videoPage is the view model for web/video.tmpl.
type videoPage struct {
	Accent   string
	ID       string
	Name     string
	State    string // ready | processing | failed | missing
	Progress int
	PosterOK bool

	SourceHuman string
	OutputHuman string
	SavedPct    int
	Duration    string
	ExpiresText string
	FileURL     string
	DownloadURL string
	ShareURL    string
}

// GET /v/{id} — the public share page. One template, four states.
func (s *server) handleVideoPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	id := r.PathValue("id")
	v, err := s.store.videoByID(id)
	if err != nil {
		s.logf("share page %s: %v", id, err)
	}

	p := videoPage{Accent: accent, ID: id}
	code := http.StatusOK

	switch {
	case v == nil || v.expired():
		p.State = "missing"
		code = http.StatusNotFound
	case v.Status == "failed":
		p.State = "failed"
		p.Name = v.OrigName
	case v.Status != "ready":
		p.State = "processing"
		p.Name = v.OrigName
		p.Progress = s.tc.progressOf(id)
	default:
		p.State = "ready"
		p.Name = v.OrigName
		p.SourceHuman = humanBytes(v.SourceBytes)
		p.OutputHuman = humanBytes(v.OutputBytes)
		if v.SourceBytes > 0 && v.OutputBytes > 0 && v.OutputBytes < v.SourceBytes {
			p.SavedPct = int(float64(v.SourceBytes-v.OutputBytes) / float64(v.SourceBytes) * 100)
		}
		p.Duration = fmtDuration(v.DurationSecs)
		p.ExpiresText = expiresText(v.ExpiresAt.Valid, v.ExpiresAt.Int64)
		p.FileURL = "/f/" + id + ".mp4"
		p.DownloadURL = "/f/" + id + ".mp4?dl=1"
		p.ShareURL = s.cfg.BaseURL + "/v/" + id
		if _, err := os.Stat(filepath.Join(s.dataDir, "videos", id, "poster.jpg")); err == nil {
			p.PosterOK = true
		}
	}

	w.WriteHeader(code)
	if err := s.tmpl.ExecuteTemplate(w, "video.tmpl", p); err != nil {
		s.logf("render share page: %v", err)
	}
}

// GET /api/v/{id}/status — minimal, public, polled by the processing page.
func (s *server) handleVideoStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	v, err := s.store.videoByID(id)
	if err != nil {
		s.fail(w, "status", err)
		return
	}
	if v == nil || v.expired() {
		writeErr(w, http.StatusNotFound, "no such video")
		return
	}
	prog := s.tc.progressOf(id)
	if v.Status == "ready" {
		prog = 100
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": v.Status, "progress": prog})
}

// GET /f/{name} — the compressed file. name is "<id>.mp4"; ?dl=1 forces a
// download rather than inline playback. http.ServeContent handles range
// requests, so seeking and streaming work.
func (s *server) handleFile(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(r.PathValue("name"), ".mp4")
	v, err := s.store.videoByID(id)
	if err != nil || v == nil || v.expired() || v.Status != "ready" {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(filepath.Join(s.dataDir, "videos", id, "out.mp4"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}

	disposition := "inline"
	if r.URL.Query().Get("dl") != "" {
		disposition = "attachment"
	}
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("%s; filename=%q", disposition, downloadName(v.OrigName)))
	w.Header().Set("Cache-Control", "public, max-age=3600")
	http.ServeContent(w, r, "out.mp4", fi.ModTime(), f)
}

// GET /t/{name} — the poster frame. name is "<id>.jpg".
func (s *server) handlePoster(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(r.PathValue("name"), ".jpg")
	v, err := s.store.videoByID(id)
	if err != nil || v == nil || v.expired() {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(filepath.Join(s.dataDir, "videos", id, "poster.jpg"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	http.ServeContent(w, r, "poster.jpg", fi.ModTime(), f)
}

// ---------- formatting ----------

var unsafeName = strings.NewReplacer("/", "-", "\\", "-", "\"", "", "\x00", "", "\n", " ", "\r", " ")

func downloadName(orig string) string {
	base := strings.TrimSuffix(orig, filepath.Ext(orig))
	base = strings.TrimSpace(unsafeName.Replace(base))
	if base == "" {
		base = "video"
	}
	if len(base) > 120 {
		base = base[:120]
	}
	return base + ".mp4"
}

func fmtDuration(secs float64) string {
	d := time.Duration(secs) * time.Second
	if d < time.Hour {
		return fmt.Sprintf("%d:%02d", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%d:%02d:%02d", int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60)
}

func expiresText(hasExpiry bool, at int64) string {
	if !hasExpiry {
		return "kept indefinitely"
	}
	left := time.Until(time.Unix(at, 0))
	switch {
	case left <= 0:
		return "expiring now"
	case left < time.Hour:
		return fmt.Sprintf("deletes in %d min", int(left.Minutes())+1)
	case left < 48*time.Hour:
		return fmt.Sprintf("deletes in %d h", int(left.Hours())+1)
	default:
		return fmt.Sprintf("deletes in %d days", int(left.Hours()/24))
	}
}
