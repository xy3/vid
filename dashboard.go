package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// retentionOption is one entry in the dropdown the client renders.
type retentionOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

func (s *server) retentionOptions() []retentionOption {
	maxRank := retentionRank(s.cfg.MaxRetention)
	if maxRank < 0 {
		maxRank = len(retentionOrder) - 1
	}
	out := make([]retentionOption, 0, maxRank+1)
	for i := 0; i <= maxRank; i++ {
		out = append(out, retentionOption{Value: retentionOrder[i], Label: retentionLabel[retentionOrder[i]]})
	}
	return out
}

// GET /api/me — answers for logged-out visitors too; the page uses it to
// decide which view to render.
func (s *server) handleMe(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"signed_in":         false,
		"retention_options": s.retentionOptions(),
	}
	if u := s.currentUser(r); u != nil {
		resp["signed_in"] = true
		resp["email"] = u.Email
		resp["name"] = u.Name
		resp["picture"] = u.Picture
		resp["default_retention"] = clampRetention(u.DefaultRetention, s.cfg.MaxRetention)
	}
	writeJSON(w, http.StatusOK, resp)
}

type videoJSON struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Status       string `json:"status"`
	Progress     int    `json:"progress"`
	Error        string `json:"error,omitempty"`
	SourceBytes  int64  `json:"source_bytes"`
	OutputBytes  int64  `json:"output_bytes"`
	DurationSecs int    `json:"duration_secs"`
	CreatedAt    int64  `json:"created_at"`
	ExpiresAt    int64  `json:"expires_at"` // 0 = never
	URL          string `json:"url"`
}

func (s *server) toJSON(v *video) videoJSON {
	prog := 0
	switch v.Status {
	case "ready":
		prog = 100
	case "processing":
		prog = s.tc.progressOf(v.ID)
	}
	var exp int64
	if v.ExpiresAt.Valid {
		exp = v.ExpiresAt.Int64
	}
	return videoJSON{
		ID: v.ID, Name: v.OrigName, Status: v.Status, Progress: prog, Error: v.Error,
		SourceBytes: v.SourceBytes, OutputBytes: v.OutputBytes,
		DurationSecs: int(v.DurationSecs + 0.5), CreatedAt: v.CreatedAt, ExpiresAt: exp,
		URL: s.cfg.BaseURL + "/v/" + v.ID,
	}
}

// GET /api/videos
func (s *server) handleVideos(w http.ResponseWriter, r *http.Request, u *user) {
	vids, err := s.store.videosByOwner(u.ID)
	if err != nil {
		s.fail(w, "list videos", err)
		return
	}
	out := make([]videoJSON, 0, len(vids))
	for _, v := range vids {
		if v.expired() {
			continue
		}
		out = append(out, s.toJSON(v))
	}
	writeJSON(w, http.StatusOK, out)
}

// POST /api/videos/{id}  {retention}  — change how long this video lives.
func (s *server) handleUpdateVideo(w http.ResponseWriter, r *http.Request, u *user) {
	var body struct {
		Retention string `json:"retention"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request body")
		return
	}
	id := r.PathValue("id")
	v, err := s.store.videoByID(id)
	if err != nil {
		s.fail(w, "load video", err)
		return
	}
	if v == nil || v.OwnerID != u.ID || v.expired() {
		writeErr(w, http.StatusNotFound, "no such video")
		return
	}
	retention := clampRetention(body.Retention, s.cfg.MaxRetention)
	exp := expiryFrom(retention, time.Now())
	ok, err := s.store.setVideoExpiry(id, u.ID, exp)
	if err != nil {
		s.fail(w, "set expiry", err)
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "no such video")
		return
	}
	var expUnix int64
	if exp.Valid {
		expUnix = exp.Int64
	}
	writeJSON(w, http.StatusOK, map[string]any{"retention": retention, "expires_at": expUnix})
}

// DELETE /api/videos/{id}
func (s *server) handleDeleteVideo(w http.ResponseWriter, r *http.Request, u *user) {
	id := r.PathValue("id")
	ok, err := s.store.deleteVideoOwned(id, u.ID)
	if err != nil {
		s.fail(w, "delete video", err)
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "no such video")
		return
	}
	_ = os.RemoveAll(filepath.Join(s.dataDir, "videos", id))
	writeJSON(w, http.StatusOK, map[string]string{"ok": "deleted"})
}

// POST /api/settings  {default_retention}
func (s *server) handleSettings(w http.ResponseWriter, r *http.Request, u *user) {
	var body struct {
		DefaultRetention string `json:"default_retention"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request body")
		return
	}
	if retentionRank(strings.TrimSpace(body.DefaultRetention)) < 0 {
		writeErr(w, http.StatusBadRequest, "unknown retention value")
		return
	}
	retention := clampRetention(body.DefaultRetention, s.cfg.MaxRetention)
	if err := s.store.setDefaultRetention(u.ID, retention); err != nil {
		s.fail(w, "save settings", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"default_retention": retention})
}
