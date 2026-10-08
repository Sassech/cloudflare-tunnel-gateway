// Package config loads and validates cf-watcher runtime configuration from
// environment variables, optionally sourced from a .env file.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

// Default values applied when the corresponding optional environment
// variable is absent or empty.
const (
	defaultDockerNetwork   = "cloudflared-network"
	defaultLogLevel        = "INFO"
	defaultDebounceSeconds = 2.0
	defaultHealthAddr      = ":9090"

	// defaultTunnelTokenFile is where cf-watcher writes the tunnel connector
	// token for cloudflared to read via --token-file. It lives on a named
	// volume shared (rw) with cf-watcher and (ro) with cloudflared.
	defaultTunnelTokenFile = "/run/cloudflared/token"

	// defaultTunnelNamePrefix is prepended to the hostname to derive the
	// default tunnel name when CF_TUNNEL_NAME is unset.
	defaultTunnelNamePrefix = "cf-watcher-"
)

// hostname is os.Hostname, swappable in tests.
var hostname = os.Hostname

// Config holds all runtime settings for cf-watcher, resolved from
// environment variables. Sensitive values such as CFAPIToken must never be
// logged in full — callers must redact before logging.
type Config struct {
	// Cloudflare API credentials (required — see Validate).
	CFAPIToken  string
	CFAccountID string

	// CFTunnelName is the name of the tunnel cf-watcher provisions (or
	// reuses, if one with this name already exists) on startup. Defaults
	// to "cf-watcher-<hostname>". Must be unique per host.
	CFTunnelName string

	// CFTunnelID, if set, pins cf-watcher to an existing tunnel and skips
	// provisioning by name entirely (escape hatch / backward compat).
	// Empty (the default) means "resolve or create by CFTunnelName".
	CFTunnelID string

	// TunnelTokenFile is the path cf-watcher writes the tunnel connector
	// token to, for cloudflared's --token-file.
	TunnelTokenFile string

	// Required only when any container uses cf.dns=true. Empty = not set.
	CFZoneID string

	// Docker integration.
	DockerNetwork string

	// Runtime behaviour.
	LogLevel        string
	DebounceSeconds float64

	// HealthAddr is the listen address for the /healthz and /metrics HTTP
	// endpoints. Set to "off" (case-insensitive) to disable the server.
	HealthAddr string

	// ReloadToken, if set, enables POST /reload and must match the
	// caller's X-Reload-Token header. Empty (the default) disables
	// /reload entirely (404) — secure by default, since reload triggers
	// real Cloudflare API calls and shares the sync loop's rate-limit
	// budget.
	ReloadToken string
}

// Load reads configuration from a .env file (if present) and then from the
// process environment, applying defaults for optional settings.
//
// Loading .env is best-effort: a missing .env file is NOT an error — this
// mirrors pydantic-settings' env_file behaviour, where the file is an
// optional source layered under real process environment variables. Any
// other error while reading an existing .env file (e.g. malformed syntax)
// is returned to the caller.
func Load() (*Config, error) {
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("config: failed to load .env file: %w", err)
	}

	cfg := &Config{
		CFAPIToken:      os.Getenv("CF_API_TOKEN"),
		CFAccountID:     os.Getenv("CF_ACCOUNT_ID"),
		CFTunnelName:    tunnelNameOrDefault(),
		CFTunnelID:      os.Getenv("CF_TUNNEL_ID"),
		TunnelTokenFile: stringOrDefault("TUNNEL_TOKEN_FILE", defaultTunnelTokenFile),
		CFZoneID:        os.Getenv("CF_ZONE_ID"),
		DockerNetwork:   stringOrDefault("DOCKER_NETWORK", defaultDockerNetwork),
		LogLevel:        stringOrDefault("LOG_LEVEL", defaultLogLevel),
		DebounceSeconds: floatOrDefault("DEBOUNCE_SECONDS", defaultDebounceSeconds),
		HealthAddr:      stringOrDefault("HEALTH_ADDR", defaultHealthAddr),
		ReloadToken:     os.Getenv("RELOAD_TOKEN"),
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// Validate enforces that all required settings are present and non-empty:
// CF_API_TOKEN, CF_ACCOUNT_ID, a tunnel name (CF_TUNNEL_NAME, or its
// hostname-derived default) and TUNNEL_TOKEN_FILE. CF_TUNNEL_ID is
// optional. All missing variables are reported together in a single error.
func (c *Config) Validate() error {
	var missing []string

	if c.CFAPIToken == "" {
		missing = append(missing, "CF_API_TOKEN")
	}
	if c.CFAccountID == "" {
		missing = append(missing, "CF_ACCOUNT_ID")
	}
	if strings.TrimSpace(c.CFTunnelName) == "" {
		missing = append(missing, "CF_TUNNEL_NAME (no default could be derived from the hostname)")
	}
	if c.TunnelTokenFile == "" {
		missing = append(missing, "TUNNEL_TOKEN_FILE")
	}

	if len(missing) > 0 {
		return fmt.Errorf("config: missing required environment variable(s): %s", strings.Join(missing, ", "))
	}

	return nil
}

// tunnelNameOrDefault returns CF_TUNNEL_NAME (trimmed) if set, otherwise
// "cf-watcher-<hostname>". Returns "" if the hostname cannot be determined,
// which Validate then reports.
func tunnelNameOrDefault() string {
	if v := strings.TrimSpace(os.Getenv("CF_TUNNEL_NAME")); v != "" {
		return v
	}
	h, err := hostname()
	if err != nil || strings.TrimSpace(h) == "" {
		return ""
	}
	return defaultTunnelNamePrefix + strings.TrimSpace(h)
}

// stringOrDefault returns the value of the environment variable key, or def
// if the variable is absent or empty.
func stringOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// floatOrDefault returns the parsed float value of the environment variable
// key, or def if the variable is absent, empty, or not a valid float.
func floatOrDefault(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}
