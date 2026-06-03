// Package config is the canonical home for jot's configuration shape and
// loader. It intentionally mirrors the legacy JotConfig struct in ad.go — new
// code (dash, future subcommands) should import this package; ad.go and
// onboard.go will migrate in a follow-up.
//
// Unlike the legacy loader, Load() returns an error rather than calling
// os.Exit, so long-running processes (the dash HTTP server, background
// pollers) can surface config problems without killing the process.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Jot is the full configuration for the jot CLI. JSON tags match the
// ~/.jot/config.json schema documented in the project README.
type Jot struct {
	AD          AD          `json:"ad"`
	ClassLink   ClassLink   `json:"classlink"`
	CortexXDR   CortexXDR   `json:"cortex_xdr"`
	IncidentIQ  IncidentIQ  `json:"incident_iq"`
	GoogleOAuth GoogleOAuth `json:"google_oauth"`
	GoogleChat  GoogleChat  `json:"google_chat"`
	Duo         Duo         `json:"duo"`
	Dash        Dash        `json:"dash"`
}

type AD struct {
	Server             string   `json:"server"`
	BaseDN             string   `json:"base_dn"`
	BindDN             string   `json:"bind_dn"`
	BindPassword       string   `json:"bind_password"`
	StaffOU            string   `json:"staff_ou"`
	StudentOU          string   `json:"student_ou"`
	DisabledOU         string   `json:"disabled_ou"`
	DefaultGroups      []string `json:"default_groups"`
	UPNSuffix          string   `json:"upn_suffix"`
	EmailSuffix        string   `json:"email_suffix"`
	DefaultPassword    string   `json:"default_password"`
	StaleThresholdDays int      `json:"stale_threshold_days"`
}

type ClassLink struct {
	SyncURL string `json:"sync_url"`
	APIKey  string `json:"api_key"`
}

type CortexXDR struct {
	ScriptPath string `json:"script_path"`
	APIKeyID   string `json:"api_key_id"`
	APIKey     string `json:"api_key"`
	FQDN       string `json:"fqdn"`
}

type IncidentIQ struct {
	BaseURL              string `json:"base_url"`
	Token                string `json:"token"`
	FieldFirstName       string `json:"field_first_name"`
	FieldLastName        string `json:"field_last_name"`
	FieldRole            string `json:"field_role"`
	ClosedStatusID       string `json:"closed_status_id"`
	SubmittedStatusID    string `json:"submitted_status_id"`
	InProgressStatusID   string `json:"in_progress_status_id"`
	OnboardingCategoryID string `json:"onboarding_category_id"`
	// MyUserID is the IIQ user GUID for the current operator. Used by the
	// dash poller to filter "tickets assigned to me". Discoverable via the
	// /users/me endpoint; if empty the dash falls back to team queue.
	MyUserID string `json:"my_user_id"`
}

type GoogleOAuth struct {
	ClientID     string `json:"client_id"`
	ProjectID    string `json:"project_id"`
	ClientSecret string `json:"client_secret"`
}

type GoogleChat struct {
	OpsSpace         string `json:"ops_space"`
	SecuritySpace    string `json:"security_space"`
	DataSpaceWebhook string `json:"data_space_webhook"`
}

type Duo struct {
	APIHostname    string `json:"api_hostname"`
	IntegrationKey string `json:"integration_key"`
	SecretKey      string `json:"secret_key"`
}

// Dash controls the embedded dashboard server. Zero-valued fields use
// sensible defaults (localhost:8787, 30-second poll).
//
// Note: browser auto-launch is governed by the `--no-open` CLI flag rather
// than a config field, because the common reason to suppress it (running
// over SSH / on a headless box) is usually a runtime choice, not a
// per-workstation preference.
type Dash struct {
	Addr        string `json:"addr"`         // "127.0.0.1:8787"
	PollSeconds int    `json:"poll_seconds"` // IIQ poll interval; default 30
}

// Dir returns the jot config directory, ~/.jot, creating it if needed.
func Dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	d := filepath.Join(home, ".jot")
	if err := os.MkdirAll(d, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", d, err)
	}
	return d, nil
}

// Path returns the absolute path to ~/.jot/config.json.
func Path() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "config.json"), nil
}

// Load reads ~/.jot/config.json, parses it, and applies environment-variable
// overrides for secrets. It never calls os.Exit — callers decide how to
// handle a missing or malformed config.
func Load() (Jot, error) {
	var cfg Jot
	path, err := Path()
	if err != nil {
		return cfg, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	applyEnvOverrides(&cfg)
	cfg.applyDefaults()
	return cfg, nil
}

// applyEnvOverrides lets the operator keep secrets out of config.json by
// setting JOT_* environment variables. Mirrors the logic in the legacy
// loadJotConfig so both code paths honor the same knobs.
func applyEnvOverrides(cfg *Jot) {
	if v := os.Getenv("JOT_AD_BIND_PASSWORD"); v != "" {
		cfg.AD.BindPassword = v
	}
	if v := os.Getenv("JOT_AD_DEFAULT_PASSWORD"); v != "" {
		cfg.AD.DefaultPassword = v
	}
	if v := os.Getenv("JOT_INCIDENT_IQ_TOKEN"); v != "" {
		cfg.IncidentIQ.Token = v
	}
	if v := os.Getenv("JOT_GOOGLE_CLIENT_SECRET"); v != "" {
		cfg.GoogleOAuth.ClientSecret = v
	}
	if v := os.Getenv("JOT_CLASSLINK_API_KEY"); v != "" {
		cfg.ClassLink.APIKey = v
	}
	if v := os.Getenv("JOT_DUO_SECRET_KEY"); v != "" {
		cfg.Duo.SecretKey = v
	}
	if v := os.Getenv("JOT_CORTEX_XDR_API_KEY"); v != "" {
		cfg.CortexXDR.APIKey = v
	}
}

func (c *Jot) applyDefaults() {
	if c.Dash.Addr == "" {
		c.Dash.Addr = "127.0.0.1:8787"
	}
	if c.Dash.PollSeconds <= 0 {
		c.Dash.PollSeconds = 30
	}
	if c.AD.StaleThresholdDays <= 0 {
		c.AD.StaleThresholdDays = 90
	}
}
