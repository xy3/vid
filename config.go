package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// config is the whole operator-facing surface: the two OAuth secrets plus a
// handful of limits that shouldn't need a rebuild to change. It lives at
// ~/.config/vid/config.json, mode 0600, written by hand — never by this
// program, so a bug in here can't clobber it.
type config struct {
	GoogleClientID     string   `json:"google_client_id"`
	GoogleClientSecret string   `json:"google_client_secret"`
	BaseURL            string   `json:"base_url"`
	AllowedEmails      []string `json:"allowed_emails"`
	MaxUploadBytes     int64    `json:"max_upload_bytes"`
	UserQuotaBytes     int64    `json:"user_quota_bytes"`
	MaxRetention       string   `json:"max_retention"`
	TranscodeTimeout   int      `json:"transcode_timeout_secs"`
}

func configPath() string {
	if p := os.Getenv("VID_CONFIG"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "vid", "config.json")
}

// allowed reports whether an email may sign in. The list is matched
// case-insensitively; an empty list means nobody, which is why loadConfig
// refuses to start with one.
func (c *config) allowed(email string) bool {
	email = strings.ToLower(strings.TrimSpace(email))
	for _, a := range c.AllowedEmails {
		if strings.ToLower(strings.TrimSpace(a)) == email {
			return true
		}
	}
	return false
}

func loadConfig() (*config, error) {
	path := configPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var c config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	if c.BaseURL == "" {
		c.BaseURL = "https://vid.1t.ie"
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	if c.MaxUploadBytes <= 0 {
		c.MaxUploadBytes = 2 << 30 // 2 GiB
	}
	if c.UserQuotaBytes <= 0 {
		c.UserQuotaBytes = 20 << 30 // 20 GiB
	}
	if c.MaxRetention == "" || retentionRank(c.MaxRetention) < 0 {
		c.MaxRetention = "forever"
	}
	if c.TranscodeTimeout <= 0 {
		c.TranscodeTimeout = 7200
	}

	// Fail loudly at startup rather than at the OAuth callback with an opaque
	// Google error, or after an upload that can never be shared.
	if c.GoogleClientID == "" || c.GoogleClientSecret == "" {
		return nil, fmt.Errorf("%s is missing google_client_id or google_client_secret", path)
	}
	if len(c.AllowedEmails) == 0 {
		return nil, fmt.Errorf("%s has an empty allowed_emails — add at least one address or nobody can sign in", path)
	}
	return &c, nil
}
