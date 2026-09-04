# vid

Drag a video onto the page, get back a link you can share. It signs you in with
Google, uploads the file in chunks, compresses it with ffmpeg, and hosts the
result at a public `/v/<id>` URL that deletes itself on a schedule you pick.

Private by design: only Google accounts on the `allowed_emails` list can sign
in and upload. Share links themselves are public — the random 10-character id is
the only secret.

## What it does

- **Drag & drop** anywhere on the page, or use the file picker. Multiple files
  queue up.
- **Chunked upload.** The browser slices each file into ~8 MB pieces and PUTs
  them in sequence, so no single request hits Cloudflare's 100 MB body limit.
  Interrupted chunks retry; a size/offset mismatch resyncs instead of
  corrupting the file.
- **Compression.** `libx264 -crf 26 -preset medium`, scaled down to 1280px on
  the long edge if larger, AAC 128k, `+faststart` so it streams. Two encodes
  run at once; the rest queue.
- **Share page.** `/v/<id>` is a self-contained player with the original name,
  the size saved, an expiry line, Download, and Copy-link. While a video is
  still transcoding the page shows a progress bar and refreshes itself — the
  link works the moment it's created.
- **Retention.** Per upload: 1 hour, 1 day, 7 days, 30 days, or forever (capped
  by `max_retention`). Changeable afterwards from the dashboard. A janitor
  sweeps expired videos — row and files — every 5 minutes.
- **Rename.** Double-click a video's name on the dashboard (or the Rename
  button). The new name shows on the dashboard, becomes the share page's
  `og:title`, and is the filename a Download gives you. `POST /api/videos/<id>`
  takes `name`, `retention`, or both.

## Running it

```sh
go build -o vid .
./vid                     # 127.0.0.1:8389
./vid -addr 127.0.0.1:9999 -data /tmp/vid-data
./vid -insecure-cookies   # local HTTP only — see below
```

Requires `ffmpeg` and `ffprobe` on `PATH`; it checks at startup and refuses to
run without them.

Config at `~/.config/vid/config.json`, mode `0600`, written by hand (see
`config.example.json`):

```json
{
  "google_client_id": "....apps.googleusercontent.com",
  "google_client_secret": "....",
  "base_url": "https://vid.1t.ie",
  "allowed_emails": ["you@gmail.com"],
  "max_upload_bytes": 2147483648,
  "user_quota_bytes": 21474836480,
  "max_retention": "forever",
  "transcode_timeout_secs": 7200
}
```

Startup fails loudly if the OAuth client id/secret or `allowed_emails` is
missing — better than an opaque error at the callback. Path is overridable with
`VID_CONFIG`.

### Google OAuth setup

Google Cloud console → APIs & Services → Credentials → **Create OAuth client
ID** → *Web application*. Authorised redirect URI:

```
https://vid.1t.ie/auth/google/callback
```

The scopes (`openid email profile`) are non-sensitive, so no verification is
needed — either add your address as a test user or publish the consent screen.

### `-insecure-cookies`

Session cookies are `Secure` by default and browsers silently drop those over
plain HTTP, so a `http://localhost` login looks like it works and never sticks.
This flag relaxes that for local development. Never use it in production.

## Layout

| File | |
| --- | --- |
| `main.go` | server wiring, routes, embedded assets, cache-busted URLs, janitor |
| `config.go` | `~/.config/vid/config.json` load + the allowlist check |
| `auth.go` | Google OAuth (code + PKCE), sessions, per-IP limiter, request gates |
| `store.go` | SQLite schema and every query |
| `upload.go` | chunked upload sessions, quota check, assembly |
| `transcode.go` | 2-worker ffmpeg pool, progress parsing, poster frame, recovery |
| `share.go` | public `/v/<id>`, `/f/<id>.mp4`, `/t/<id>.jpg`, status |
| `dashboard.go` | `/api/me`, `/api/videos`, retention change, delete, settings |
| `util.go` | ids, retention math, JSON/byte helpers |
| `web/` | embedded UI; `video.tmpl` is the server-rendered share page |

Go standard library plus `modernc.org/sqlite` (pure Go, no cgo — the binary
stays static). Everything else, including the OAuth flow, is stdlib HTTP.

## Data

```
data/
  vid.db                  users, sessions, videos
  tmp/<upload>.part        in-flight chunked uploads (swept after 2h idle)
  videos/<id>/out.mp4      the compressed result
  videos/<id>/poster.jpg   thumbnail
```

The source file is deleted once compression succeeds — only `out.mp4` and the
poster are kept. `data/` should be `chmod 700` (it holds other people's
videos); Caddy reaches this app through `reverse_proxy` and never needs to read
the directory.
