package main

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"flag"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

// accent is this app's one colour, matching the 1t.ie family convention of a
// single accent per tile. Referenced by the share-page template too.
const accent = "#4db8ff"

type server struct {
	cfg           *config
	store         *store
	tc            *transcoder
	up            *uploader
	tmpl          *template.Template
	dataDir       string
	logger        *log.Logger
	limiter       *limiter
	secureCookies bool
}

func (s *server) logf(format string, args ...any) { s.logger.Printf(format, args...) }

// fail logs the real error and returns a generic one — internal errors leak
// schema and filesystem detail, and the log is where that belongs.
func (s *server) fail(w http.ResponseWriter, what string, err error) {
	s.logf("%s: %v", what, err)
	writeErr(w, http.StatusInternalServerError, what+" failed")
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8389", "listen address")
	dataDir := flag.String("data", "", "data directory (default ./data)")
	insecureCookies := flag.Bool("insecure-cookies", false, "allow session cookies over plain HTTP (local dev only)")
	flag.Parse()

	logger := log.New(os.Stdout, "", log.LstdFlags)

	if err := checkFFTools(); err != nil {
		logger.Fatalf("ffmpeg: %v", err)
	}
	if err := buildIndex(); err != nil {
		logger.Fatalf("index: %v", err)
	}

	tmpl, err := template.ParseFS(webFS, "web/video.tmpl")
	if err != nil {
		logger.Fatalf("template: %v", err)
	}

	cfg, err := loadConfig()
	if err != nil {
		logger.Fatalf("config: %v", err)
	}

	dir := *dataDir
	if dir == "" {
		wd, _ := os.Getwd()
		dir = filepath.Join(wd, "data")
	}
	for _, sub := range []string{"videos", "tmp"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			logger.Fatalf("data dir: %v", err)
		}
	}

	st, err := openStore(filepath.Join(dir, "vid.db"))
	if err != nil {
		logger.Fatalf("store: %v", err)
	}
	defer st.Close()

	s := &server{
		cfg:           cfg,
		store:         st,
		tmpl:          tmpl,
		dataDir:       dir,
		logger:        logger,
		limiter:       newLimiter(),
		secureCookies: !*insecureCookies,
		up:            newUploader(dir, cfg.MaxUploadBytes),
	}
	s.tc = newTranscoder(st, dir, time.Duration(cfg.TranscodeTimeout)*time.Second, logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	s.tc.start(ctx)
	go s.janitor(ctx)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No ReadTimeout/WriteTimeout: uploads and downloads of multi-GB files
		// legitimately hold a connection for a long time. Per-chunk size caps
		// and the storage quota are what bound resource use here.
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	logger.Printf("vid listening on %s  (base %s)", *addr, cfg.BaseURL)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Fatalf("listen: %v", err)
	}
	logger.Printf("vid stopped")
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	static, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheHeaders(http.FileServer(http.FS(static)))))
	mux.HandleFunc("GET /favicon.svg", s.serveAsset("favicon.svg", "image/svg+xml"))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })

	mux.HandleFunc("GET /{$}", s.handleIndex)

	// Auth
	mux.HandleFunc("GET /auth/google/start", s.handleGoogleStart)
	mux.HandleFunc("GET /auth/google/callback", s.handleGoogleCallback)
	mux.HandleFunc("POST /auth/logout", s.handleLogout)
	mux.HandleFunc("GET /auth/logout", s.handleLogout)

	// Dashboard API
	mux.HandleFunc("GET /api/me", s.handleMe)
	mux.HandleFunc("GET /api/videos", s.requireUser(s.handleVideos))
	mux.HandleFunc("POST /api/videos/{id}", s.requireUser(s.handleUpdateVideo))
	mux.HandleFunc("DELETE /api/videos/{id}", s.requireUser(s.handleDeleteVideo))
	mux.HandleFunc("POST /api/settings", s.requireUser(s.handleSettings))

	// Chunked upload
	mux.HandleFunc("POST /api/uploads", s.requireUser(s.handleCreateUpload))
	mux.HandleFunc("PUT /api/uploads/{id}", s.requireUser(s.handlePutChunk))
	mux.HandleFunc("POST /api/uploads/{id}/finish", s.requireUser(s.handleFinishUpload))
	mux.HandleFunc("DELETE /api/uploads/{id}", s.requireUser(s.handleAbortUpload))

	// Public share surface
	mux.HandleFunc("GET /v/{id}", s.handleVideoPage)
	mux.HandleFunc("GET /api/v/{id}/status", s.handleVideoStatus)
	mux.HandleFunc("GET /f/{name}", s.handleFile)
	mux.HandleFunc("GET /t/{name}", s.handlePoster)

	return securityHeaders(mux)
}

// securityHeaders mirrors brano's: a strict CSP as a second line of defence.
// Google sign-in is a full-page redirect, so 'self' everywhere is fine.
func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data: blob:; media-src 'self' blob:; "+
				"style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'; "+
				"frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.ServeHTTP(w, r)
	})
}

func (s *server) serveAsset(name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := webFS.ReadFile("web/" + name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(b)
	}
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(indexHTML)
}

// renderNotice is the small standalone page used for OAuth dead-ends (not on
// the allowlist, link expired, and so on).
func (s *server) renderNotice(w http.ResponseWriter, code int, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	_ = s.tmpl.ExecuteTemplate(w, "video.tmpl", videoPage{
		Accent: accent, State: "notice", Name: title, ExpiresText: body,
	})
}

// ---------- cache-busted index (copied from brano/main.go) ----------
//
// Cloudflare edge-caches .css and .js by extension for over two weeks whatever
// max-age we send. buildIndex() stamps a hash of the assets into their URLs so
// a rebuilt binary serves a cache key no browser has seen; cacheHeaders() sends
// no-cache so the edge revalidates rather than serving stale.

var indexHTML []byte

func buildIndex() error {
	raw, err := webFS.ReadFile("web/index.html")
	if err != nil {
		return err
	}
	h := sha256.New()
	for _, f := range []string{"web/app.js", "web/style.css"} {
		b, err := webFS.ReadFile(f)
		if err != nil {
			return err
		}
		h.Write(b)
	}
	stamp := hex.EncodeToString(h.Sum(nil))[:8]
	out := strings.ReplaceAll(string(raw), "/static/app.js", "/static/app.js?v="+stamp)
	out = strings.ReplaceAll(out, "/static/style.css", "/static/style.css?v="+stamp)
	indexHTML = []byte(out)
	return nil
}

func cacheHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".woff2") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		h.ServeHTTP(w, r)
	})
}

// ---------- janitor ----------

// janitor enforces retention: expired videos are removed row and file, aborted
// uploads are swept, and long-failed rows are cleared so they stop showing up.
func (s *server) janitor(ctx context.Context) {
	tick := time.NewTicker(5 * time.Minute)
	defer tick.Stop()
	s.sweepOnce()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.sweepOnce()
		}
	}
}

func (s *server) sweepOnce() {
	now := time.Now().Unix()

	ids, err := s.store.expiredIDs(now)
	if err != nil {
		s.logf("janitor: expired lookup: %v", err)
	}
	for _, id := range ids {
		_ = os.RemoveAll(filepath.Join(s.dataDir, "videos", id))
		if err := s.store.deleteVideo(id); err != nil {
			s.logf("janitor: delete %s: %v", id, err)
		}
	}
	if len(ids) > 0 {
		s.logf("janitor: removed %d expired video(s)", len(ids))
	}

	stale, err := s.store.failedIDsBefore(now - int64((24 * time.Hour).Seconds()))
	if err != nil {
		s.logf("janitor: failed lookup: %v", err)
	}
	for _, id := range stale {
		_ = os.RemoveAll(filepath.Join(s.dataDir, "videos", id))
		_ = s.store.deleteVideo(id)
	}

	s.up.sweep()
}
