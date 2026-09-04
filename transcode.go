package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// This box has 4 cores. Two concurrent x264 encodes leaves headroom for
	// Caddy, the tunnel, and everything else on it.
	transcodeWorkers = 2
	// Target ceiling for the long edge. Anything already smaller is left alone
	// by the scale filter's min() expression.
	maxWidth = 1280
)

// transcoder is a small worker pool that turns an uploaded source file into a
// web-friendly out.mp4 plus a poster frame. Progress is kept in memory and read
// by status polls; the durable state is the videos row.
type transcoder struct {
	store   *store
	dataDir string
	logger  *log.Logger
	timeout time.Duration

	queue chan string

	mu       sync.Mutex
	progress map[string]int
}

func newTranscoder(st *store, dataDir string, timeout time.Duration, logger *log.Logger) *transcoder {
	return &transcoder{
		store:    st,
		dataDir:  dataDir,
		logger:   logger,
		timeout:  timeout,
		queue:    make(chan string, 256),
		progress: map[string]int{},
	}
}

func (t *transcoder) logf(format string, args ...any) { t.logger.Printf(format, args...) }

// start launches the workers and re-queues anything left unfinished by a
// previous run. A row stuck in "processing" means we died mid-encode; reset it
// so a worker picks it up cleanly.
func (t *transcoder) start(ctx context.Context) {
	for i := 0; i < transcodeWorkers; i++ {
		go t.worker(ctx)
	}
	pending, err := t.store.unfinished()
	if err != nil {
		t.logf("recover: %v", err)
		return
	}
	for _, v := range pending {
		if v.Status == "processing" {
			_ = t.store.setVideoStatus(v.ID, "queued", "")
		}
		t.enqueue(v.ID)
	}
	if len(pending) > 0 {
		t.logf("recover: re-queued %d unfinished video(s)", len(pending))
	}
}

// enqueue never blocks the caller: the buffer is large, and on the rare
// overflow a goroutine waits instead of the HTTP handler.
func (t *transcoder) enqueue(id string) {
	select {
	case t.queue <- id:
	default:
		go func() { t.queue <- id }()
	}
}

func (t *transcoder) setProgress(id string, pct int) {
	t.mu.Lock()
	t.progress[id] = pct
	t.mu.Unlock()
}

func (t *transcoder) clearProgress(id string) {
	t.mu.Lock()
	delete(t.progress, id)
	t.mu.Unlock()
}

// progressOf returns a percentage for a video still being worked on. Callers
// treat a ready row as 100 and a missing entry as 0.
func (t *transcoder) progressOf(id string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.progress[id]
}

func (t *transcoder) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-t.queue:
			t.process(ctx, id)
		}
	}
}

func (t *transcoder) process(parentCtx context.Context, id string) {
	v, err := t.store.videoByID(id)
	if err != nil || v == nil {
		return
	}
	if v.Status != "queued" {
		return // already handled, or deleted
	}

	dir := filepath.Join(t.dataDir, "videos", id)
	src, err := findSource(dir)
	if err != nil {
		t.fail(id, "source file missing")
		return
	}

	_ = t.store.setVideoStatus(id, "processing", "")
	t.setProgress(id, 0)
	defer t.clearProgress(id)

	ctx, cancel := context.WithTimeout(parentCtx, t.timeout)
	defer cancel()

	dur, wds, hgt, err := ffprobe(ctx, src)
	if err != nil {
		t.fail(id, "not a video file")
		_ = os.Remove(src)
		return
	}
	_ = t.store.setVideoProbe(id, dur, wds, hgt)

	out := filepath.Join(dir, "out.mp4")
	if err := t.encode(ctx, src, out, dur, id); err != nil {
		_ = os.Remove(out)
		if ctx.Err() == context.DeadlineExceeded {
			t.fail(id, "took too long to compress")
		} else {
			t.fail(id, "compression failed")
			t.logf("encode %s: %v", id, err)
		}
		_ = os.Remove(src)
		return
	}

	fi, err := os.Stat(out)
	if err != nil {
		t.fail(id, "compression produced no file")
		_ = os.Remove(src)
		return
	}

	// Re-probe the output: the source dimensions were scaled down, and link
	// unfurlers (Discord, Slack, iMessage) need the real width/height of the
	// file they'll embed.
	if _, ow, oh, perr := ffprobe(ctx, out); perr == nil && ow > 0 && oh > 0 {
		_ = t.store.setVideoProbe(id, dur, ow, oh)
	}

	// Best effort — a missing poster just means the share page shows the first
	// frame the browser decodes.
	if err := poster(ctx, out, filepath.Join(dir, "poster.jpg"), dur); err != nil {
		t.logf("poster %s: %v", id, err)
	}

	if err := t.store.setVideoReady(id, fi.Size()); err != nil {
		t.logf("mark ready %s: %v", id, err)
	}
	_ = os.Remove(src) // keep only out.mp4 + poster.jpg
	t.setProgress(id, 100)
	t.logf("ready %s: %s -> %s", id, humanBytes(v.SourceBytes), humanBytes(fi.Size()))
}

func (t *transcoder) fail(id, msg string) {
	if err := t.store.setVideoStatus(id, "failed", msg); err != nil {
		t.logf("mark failed %s: %v", id, err)
	}
}

func findSource(dir string) (string, error) {
	matches, _ := filepath.Glob(filepath.Join(dir, "source.*"))
	if len(matches) == 0 {
		return "", fmt.Errorf("no source in %s", dir)
	}
	return matches[0], nil
}

// ---------- ffmpeg / ffprobe ----------

// checkFFTools fails fast at startup if the binaries this whole app depends on
// aren't installed, rather than surfacing as a mysterious failed upload later.
func checkFFTools() error {
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("%s not found on PATH", bin)
		}
	}
	return nil
}

func ffprobe(ctx context.Context, path string) (durSecs float64, width, height int, err error) {
	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height",
		"-show_entries", "format=duration",
		"-of", "json", path)
	buf, err := cmd.Output()
	if err != nil {
		return 0, 0, 0, err
	}
	var probe struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(buf, &probe); err != nil {
		return 0, 0, 0, err
	}
	if len(probe.Streams) == 0 {
		return 0, 0, 0, fmt.Errorf("no video stream")
	}
	d, _ := strconv.ParseFloat(strings.TrimSpace(probe.Format.Duration), 64)
	return d, probe.Streams[0].Width, probe.Streams[0].Height, nil
}

// encode runs the compression pass, streaming -progress on stdout so the UI can
// show a live bar. ffmpeg spawns no children, so CommandContext's kill is
// enough to stop it on timeout or shutdown.
func (t *transcoder) encode(ctx context.Context, src, out string, durSecs float64, id string) error {
	args := []string{
		"-y", "-i", src,
		"-c:v", "libx264", "-preset", "medium", "-crf", "26",
		"-vf", fmt.Sprintf("scale='min(%d,iw)':-2", maxWidth),
		"-c:a", "aac", "-b:a", "128k", "-ac", "2",
		"-movflags", "+faststart", "-pix_fmt", "yuv420p",
		"-progress", "pipe:1", "-nostats",
		out,
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var errTail ringBuffer
	cmd.Stderr = &errTail

	if err := cmd.Start(); err != nil {
		return err
	}

	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		line := sc.Text()
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if key == "out_time_us" && durSecs > 0 {
			if us, err := strconv.ParseFloat(strings.TrimSpace(val), 64); err == nil {
				pct := int(us / 1e6 / durSecs * 100)
				if pct < 0 {
					pct = 0
				}
				if pct > 99 {
					pct = 99
				}
				t.setProgress(id, pct)
			}
		}
	}

	if err := cmd.Wait(); err != nil {
		tail := strings.TrimSpace(errTail.String())
		if tail != "" {
			return fmt.Errorf("%w: %s", err, lastLine(tail))
		}
		return err
	}
	return nil
}

func poster(ctx context.Context, src, out string, durSecs float64) error {
	at := 1.0
	if durSecs > 0 && durSecs/2 < at {
		at = durSecs / 2
	}
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-y", "-ss", strconv.FormatFloat(at, 'f', 2, 64), "-i", src,
		"-frames:v", "1", "-vf", "scale=640:-2", out)
	return cmd.Run()
}

// ---------- small helpers ----------

func lastLine(s string) string {
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// ringBuffer keeps only the last few KB written to it — enough of ffmpeg's
// stderr to explain a failure, without buffering an entire noisy run.
type ringBuffer struct {
	buf []byte
	max int
}

func (r *ringBuffer) Write(p []byte) (int, error) {
	if r.max == 0 {
		r.max = 4096
	}
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.max {
		r.buf = r.buf[len(r.buf)-r.max:]
	}
	return len(p), nil
}

func (r *ringBuffer) String() string { return string(r.buf) }
