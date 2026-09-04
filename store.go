package main

import (
	"database/sql"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS users (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  google_sub        TEXT NOT NULL UNIQUE,
  email             TEXT NOT NULL,
  name              TEXT NOT NULL DEFAULT '',
  picture           TEXT NOT NULL DEFAULT '',
  default_retention TEXT NOT NULL DEFAULT '7d',
  created_at        INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
  token      TEXT PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id),
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS videos (
  id            TEXT PRIMARY KEY,
  owner_id      INTEGER NOT NULL REFERENCES users(id),
  orig_name     TEXT NOT NULL,
  status        TEXT NOT NULL,           -- queued | processing | ready | failed
  error         TEXT NOT NULL DEFAULT '',
  source_bytes  INTEGER NOT NULL DEFAULT 0,
  output_bytes  INTEGER NOT NULL DEFAULT 0,
  duration_secs REAL NOT NULL DEFAULT 0,
  width         INTEGER NOT NULL DEFAULT 0,
  height        INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL,
  ready_at      INTEGER,
  expires_at    INTEGER                  -- NULL = keep forever
);

CREATE INDEX IF NOT EXISTS idx_videos_owner  ON videos(owner_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_videos_expiry ON videos(expires_at);
`

type store struct{ db *sql.DB }

func openStore(path string) (*store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	// modernc's driver dislikes concurrent writers; one connection plus WAL is
	// plenty here and sidesteps SQLITE_BUSY between the janitor, the transcode
	// workers, and HTTP handlers.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	return &store{db: db}, nil
}

func (s *store) Close() error { return s.db.Close() }

func nowUnix() int64 { return time.Now().Unix() }

// ---------- users ----------

type user struct {
	ID               int64  `json:"-"`
	GoogleSub        string `json:"-"`
	Email            string `json:"email"`
	Name             string `json:"name"`
	Picture          string `json:"picture"`
	DefaultRetention string `json:"default_retention"`
}

const userCols = `id, google_sub, email, name, picture, default_retention`

func scanUser(row interface{ Scan(...any) error }) (*user, error) {
	var u user
	if err := row.Scan(&u.ID, &u.GoogleSub, &u.Email, &u.Name, &u.Picture, &u.DefaultRetention); err != nil {
		return nil, err
	}
	return &u, nil
}

// upsertUser records the Google account, keying on the stable `sub` claim so a
// changed display name or avatar updates in place and an email change doesn't
// fork the account.
func (s *store) upsertUser(sub, email, name, picture string) (*user, error) {
	_, err := s.db.Exec(
		`INSERT INTO users (google_sub, email, name, picture, created_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(google_sub) DO UPDATE SET
		   email = excluded.email, name = excluded.name, picture = excluded.picture`,
		sub, email, name, picture, nowUnix())
	if err != nil {
		return nil, err
	}
	return scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users WHERE google_sub = ?`, sub))
}

func (s *store) userByID(id int64) (*user, error) {
	return scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

func (s *store) setDefaultRetention(id int64, retention string) error {
	_, err := s.db.Exec(`UPDATE users SET default_retention = ? WHERE id = ?`, retention, id)
	return err
}

// ---------- sessions ----------

func (s *store) createSession(token string, userID int64, ttl time.Duration) error {
	now := time.Now()
	_, err := s.db.Exec(
		`INSERT INTO sessions (token, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		token, userID, now.Unix(), now.Add(ttl).Unix())
	return err
}

// sessionUser resolves a session token to its user, treating an expired row as
// absence and clearing it on the way out.
func (s *store) sessionUser(token string) (*user, error) {
	var id, exp int64
	err := s.db.QueryRow(`SELECT user_id, expires_at FROM sessions WHERE token = ?`, token).Scan(&id, &exp)
	if err != nil {
		return nil, err
	}
	if time.Now().Unix() > exp {
		_ = s.deleteSession(token)
		return nil, sql.ErrNoRows
	}
	return s.userByID(id)
}

func (s *store) deleteSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token = ?`, token)
	return err
}

// ---------- videos ----------

type video struct {
	ID           string
	OwnerID      int64
	OrigName     string
	Status       string
	Error        string
	SourceBytes  int64
	OutputBytes  int64
	DurationSecs float64
	Width        int
	Height       int
	CreatedAt    int64
	ReadyAt      sql.NullInt64
	ExpiresAt    sql.NullInt64
}

func (v *video) expired() bool {
	return v.ExpiresAt.Valid && time.Now().Unix() > v.ExpiresAt.Int64
}

const videoCols = `id, owner_id, orig_name, status, error, source_bytes, output_bytes,
	duration_secs, width, height, created_at, ready_at, expires_at`

func scanVideo(row interface{ Scan(...any) error }) (*video, error) {
	var v video
	err := row.Scan(&v.ID, &v.OwnerID, &v.OrigName, &v.Status, &v.Error,
		&v.SourceBytes, &v.OutputBytes, &v.DurationSecs, &v.Width, &v.Height,
		&v.CreatedAt, &v.ReadyAt, &v.ExpiresAt)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func (s *store) insertVideo(v *video) error {
	_, err := s.db.Exec(
		`INSERT INTO videos (id, owner_id, orig_name, status, source_bytes, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		v.ID, v.OwnerID, v.OrigName, v.Status, v.SourceBytes, v.CreatedAt, v.ExpiresAt)
	return err
}

func (s *store) videoByID(id string) (*video, error) {
	v, err := scanVideo(s.db.QueryRow(`SELECT `+videoCols+` FROM videos WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return v, err
}

func (s *store) videosByOwner(ownerID int64) ([]*video, error) {
	rows, err := s.db.Query(`SELECT `+videoCols+` FROM videos WHERE owner_id = ? ORDER BY created_at DESC`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*video
	for rows.Next() {
		v, err := scanVideo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *store) setVideoProbe(id string, dur float64, w, h int) error {
	_, err := s.db.Exec(`UPDATE videos SET duration_secs = ?, width = ?, height = ? WHERE id = ?`, dur, w, h, id)
	return err
}

func (s *store) setVideoStatus(id, status, errMsg string) error {
	_, err := s.db.Exec(`UPDATE videos SET status = ?, error = ? WHERE id = ?`, status, errMsg, id)
	return err
}

func (s *store) setVideoReady(id string, outputBytes int64) error {
	_, err := s.db.Exec(
		`UPDATE videos SET status = 'ready', error = '', output_bytes = ?, ready_at = ? WHERE id = ?`,
		outputBytes, nowUnix(), id)
	return err
}

// setVideoExpiry is owner-scoped so a guessed id can't retention-bomb someone
// else's video. Returns whether a row matched.
func (s *store) setVideoExpiry(id string, ownerID int64, expiresAt sql.NullInt64) (bool, error) {
	res, err := s.db.Exec(`UPDATE videos SET expires_at = ? WHERE id = ? AND owner_id = ?`, expiresAt, id, ownerID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *store) deleteVideoOwned(id string, ownerID int64) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM videos WHERE id = ? AND owner_id = ?`, id, ownerID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *store) deleteVideo(id string) error {
	_, err := s.db.Exec(`DELETE FROM videos WHERE id = ?`, id)
	return err
}

// ownerUsage is the sum of source bytes for anything still processing plus
// output bytes for anything finished — i.e. what's actually on disk for this
// user right now, which is what the quota is about.
func (s *store) ownerUsage(ownerID int64) (int64, error) {
	var n sql.NullInt64
	err := s.db.QueryRow(
		`SELECT COALESCE(SUM(CASE WHEN status = 'ready' THEN output_bytes ELSE source_bytes END), 0)
		 FROM videos WHERE owner_id = ?`, ownerID).Scan(&n)
	return n.Int64, err
}

// unfinished returns everything the transcoder still owes work on, used once at
// startup to recover from a crash or restart mid-encode.
func (s *store) unfinished() ([]*video, error) {
	rows, err := s.db.Query(`SELECT ` + videoCols + ` FROM videos WHERE status IN ('queued', 'processing')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*video
	for rows.Next() {
		v, err := scanVideo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *store) expiredIDs(now int64) ([]string, error) {
	return s.idQuery(`SELECT id FROM videos WHERE expires_at IS NOT NULL AND expires_at < ?`, now)
}

func (s *store) failedIDsBefore(cutoff int64) ([]string, error) {
	return s.idQuery(`SELECT id FROM videos WHERE status = 'failed' AND created_at < ?`, cutoff)
}

func (s *store) idQuery(q string, args ...any) ([]string, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
