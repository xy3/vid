package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ---------- ids and tokens ----------

// randomToken is a 256-bit URL-safe string, used for session and upload ids.
func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

const idAlphabet = "0123456789abcdefghjkmnpqrstvwxyz" // crockford base32, no i/l/o/u

// newVideoID is the public handle in /v/<id>. 10 chars of this alphabet is ~50
// bits — not guessable, and short enough to paste into a chat.
func newVideoID() string {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	for i := range b {
		b[i] = idAlphabet[int(b[i])%len(idAlphabet)]
	}
	return string(b)
}

// ---------- retention ----------

// retentionOrder is shortest-lived first; the index doubles as a rank for
// clamping a requested value to the operator's configured maximum.
var retentionOrder = []string{"1h", "24h", "7d", "30d", "forever"}

var retentionDur = map[string]time.Duration{
	"1h":      time.Hour,
	"24h":     24 * time.Hour,
	"7d":      7 * 24 * time.Hour,
	"30d":     30 * 24 * time.Hour,
	"forever": 0,
}

var retentionLabel = map[string]string{
	"1h":      "1 hour",
	"24h":     "1 day",
	"7d":      "7 days",
	"30d":     "30 days",
	"forever": "forever",
}

func retentionRank(s string) int {
	for i, v := range retentionOrder {
		if v == s {
			return i
		}
	}
	return -1
}

// clampRetention resolves a client-supplied value to something valid and no
// longer-lived than max. An unknown request falls back to 7d.
func clampRetention(req, max string) string {
	r := retentionRank(req)
	if r < 0 {
		r = retentionRank("7d")
	}
	m := retentionRank(max)
	if m < 0 {
		m = len(retentionOrder) - 1
	}
	if r > m {
		r = m
	}
	return retentionOrder[r]
}

// expiryFrom turns a retention value into an absolute deadline. "forever"
// (duration 0) yields a NULL expires_at.
func expiryFrom(retention string, from time.Time) sql.NullInt64 {
	d := retentionDur[retention]
	if d == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: from.Add(d).Unix(), Valid: true}
}

// ---------- http helpers ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

// humanBytes formats a size the way a share page should show it.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
