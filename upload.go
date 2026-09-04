package main

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// chunkSize is what the browser is told to slice files into. Kept well under
// Cloudflare's 100 MB per-request body limit, which is the whole reason uploads
// are chunked rather than a single multipart POST.
const chunkSize = 8 << 20 // 8 MiB

// maxChunkBytes bounds a single PUT body — chunkSize plus slack, so a
// misbehaving client can't stream something huge into one request.
const maxChunkBytes = chunkSize + (1 << 20)

// uploadSessionTTL is how long an idle half-finished upload is kept before the
// sweeper reclaims its .part file.
const uploadSessionTTL = 2 * time.Hour

var videoExt = map[string]bool{
	".mp4": true, ".mov": true, ".m4v": true, ".webm": true, ".mkv": true,
	".avi": true, ".wmv": true, ".flv": true, ".mpg": true, ".mpeg": true,
	".ts": true, ".3gp": true, ".ogv": true, ".mts": true, ".m2ts": true,
}

type uploadSession struct {
	ID        string
	UserID    int64
	Name      string
	Size      int64
	Retention string
	Part      string // path to the .part file

	mu       sync.Mutex
	Received int64
	LastSeen time.Time
}

type uploader struct {
	dataDir  string
	maxBytes int64

	mu       sync.Mutex
	sessions map[string]*uploadSession
}

func newUploader(dataDir string, maxBytes int64) *uploader {
	return &uploader{
		dataDir:  dataDir,
		maxBytes: maxBytes,
		sessions: map[string]*uploadSession{},
	}
}

func (u *uploader) tmpDir() string { return filepath.Join(u.dataDir, "tmp") }

func (u *uploader) get(id string, userID int64) *uploadSession {
	u.mu.Lock()
	defer u.mu.Unlock()
	s := u.sessions[id]
	if s == nil || s.UserID != userID {
		return nil
	}
	return s
}

func (u *uploader) drop(s *uploadSession) {
	u.mu.Lock()
	delete(u.sessions, s.ID)
	u.mu.Unlock()
	_ = os.Remove(s.Part)
}

// sweep discards uploads nobody has touched in a while. A process restart also
// clears them (the map is in memory); this handles the tab-left-open case.
func (u *uploader) sweep() {
	cutoff := time.Now().Add(-uploadSessionTTL)
	u.mu.Lock()
	var stale []*uploadSession
	for _, s := range u.sessions {
		s.mu.Lock()
		if s.LastSeen.Before(cutoff) {
			stale = append(stale, s)
		}
		s.mu.Unlock()
	}
	for _, s := range stale {
		delete(u.sessions, s.ID)
	}
	u.mu.Unlock()
	for _, s := range stale {
		_ = os.Remove(s.Part)
	}
	// Also clear orphaned .part files with no live session (e.g. left by a
	// crash mid-write).
	entries, _ := os.ReadDir(u.tmpDir())
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".part") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".part")
		u.mu.Lock()
		_, live := u.sessions[id]
		u.mu.Unlock()
		if !live {
			_ = os.Remove(filepath.Join(u.tmpDir(), e.Name()))
		}
	}
}

// ---------- HTTP handlers ----------

// POST /api/uploads  {name, size, retention}  ->  {upload_id, chunk_size}
func (s *server) handleCreateUpload(w http.ResponseWriter, r *http.Request, cu *user) {
	var body struct {
		Name      string `json:"name"`
		Size      int64  `json:"size"`
		Retention string `json:"retention"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request body")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = "video"
	}
	if len(name) > 300 {
		name = name[:300]
	}
	if body.Size <= 0 {
		writeErr(w, http.StatusBadRequest, "size must be positive")
		return
	}
	if body.Size > s.up.maxBytes {
		writeErr(w, http.StatusRequestEntityTooLarge,
			"file is larger than the "+humanBytes(s.up.maxBytes)+" limit")
		return
	}

	used, err := s.store.ownerUsage(cu.ID)
	if err != nil {
		s.fail(w, "usage check", err)
		return
	}
	if used+body.Size > s.cfg.UserQuotaBytes {
		writeErr(w, http.StatusInsufficientStorage,
			"that would put you over your "+humanBytes(s.cfg.UserQuotaBytes)+" storage quota")
		return
	}

	retention := clampRetention(body.Retention, s.cfg.MaxRetention)

	if err := os.MkdirAll(s.up.tmpDir(), 0o700); err != nil {
		s.fail(w, "tmp dir", err)
		return
	}
	id := randomToken()
	part := filepath.Join(s.up.tmpDir(), id+".part")
	f, err := os.OpenFile(part, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		s.fail(w, "create part", err)
		return
	}
	_ = f.Close()

	sess := &uploadSession{
		ID: id, UserID: cu.ID, Name: name, Size: body.Size,
		Retention: retention, Part: part, LastSeen: time.Now(),
	}
	s.up.mu.Lock()
	s.up.sessions[id] = sess
	s.up.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"upload_id":  id,
		"chunk_size": chunkSize,
		"retention":  retention,
	})
}

// PUT /api/uploads/{id}?offset=N   (raw chunk body)  ->  {received}
func (s *server) handlePutChunk(w http.ResponseWriter, r *http.Request, cu *user) {
	sess := s.up.get(r.PathValue("id"), cu.ID)
	if sess == nil {
		writeErr(w, http.StatusNotFound, "no such upload")
		return
	}
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil || offset < 0 {
		writeErr(w, http.StatusBadRequest, "offset required")
		return
	}

	sess.mu.Lock()
	defer sess.mu.Unlock()

	if offset != sess.Received {
		// The client and server disagree on progress — tell the client where to
		// resume rather than corrupting the file.
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "offset mismatch", "expected": sess.Received,
		})
		return
	}

	f, err := os.OpenFile(sess.Part, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		s.fail(w, "open part", err)
		return
	}
	defer f.Close()

	n, err := io.Copy(f, io.LimitReader(r.Body, maxChunkBytes))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "chunk write interrupted")
		return
	}
	if sess.Received+n > sess.Size {
		// More bytes than declared — abandon it.
		s.up.drop(sess)
		writeErr(w, http.StatusBadRequest, "upload exceeded declared size")
		return
	}
	sess.Received += n
	sess.LastSeen = time.Now()

	writeJSON(w, http.StatusOK, map[string]any{"received": sess.Received})
}

// POST /api/uploads/{id}/finish  ->  {id, url}
func (s *server) handleFinishUpload(w http.ResponseWriter, r *http.Request, cu *user) {
	sess := s.up.get(r.PathValue("id"), cu.ID)
	if sess == nil {
		writeErr(w, http.StatusNotFound, "no such upload")
		return
	}
	sess.mu.Lock()
	received, size := sess.Received, sess.Size
	sess.mu.Unlock()

	if received != size {
		writeErr(w, http.StatusBadRequest, "upload incomplete")
		return
	}

	// Allocate the public id and its directory, then move the assembled file in
	// as source.<ext>. The extension is cosmetic — ffprobe decides what it
	// actually is — but a real one keeps the data dir legible.
	vidID := newVideoID()
	for {
		existing, _ := s.store.videoByID(vidID)
		if existing == nil {
			break
		}
		vidID = newVideoID()
	}
	dir := filepath.Join(s.dataDir, "videos", vidID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		s.fail(w, "video dir", err)
		return
	}
	ext := strings.ToLower(filepath.Ext(sess.Name))
	if !videoExt[ext] {
		ext = ".bin"
	}
	srcPath := filepath.Join(dir, "source"+ext)
	if err := os.Rename(sess.Part, srcPath); err != nil {
		// Rename can fail across filesystems; fall back to a copy.
		if cpErr := copyFile(sess.Part, srcPath); cpErr != nil {
			_ = os.RemoveAll(dir)
			s.fail(w, "store source", cpErr)
			return
		}
		_ = os.Remove(sess.Part)
	}

	v := &video{
		ID: vidID, OwnerID: cu.ID, OrigName: sess.Name, Status: "queued",
		SourceBytes: size, CreatedAt: nowUnix(),
		ExpiresAt: expiryFrom(sess.Retention, time.Now()),
	}
	if err := s.store.insertVideo(v); err != nil {
		_ = os.RemoveAll(dir)
		s.fail(w, "save video", err)
		return
	}

	s.up.mu.Lock()
	delete(s.up.sessions, sess.ID)
	s.up.mu.Unlock()

	s.tc.enqueue(vidID)

	writeJSON(w, http.StatusOK, map[string]any{
		"id":  vidID,
		"url": s.cfg.BaseURL + "/v/" + vidID,
	})
}

// DELETE /api/uploads/{id}
func (s *server) handleAbortUpload(w http.ResponseWriter, r *http.Request, cu *user) {
	sess := s.up.get(r.PathValue("id"), cu.ID)
	if sess == nil {
		writeErr(w, http.StatusNotFound, "no such upload")
		return
	}
	s.up.drop(sess)
	writeJSON(w, http.StatusOK, map[string]string{"ok": "aborted"})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
