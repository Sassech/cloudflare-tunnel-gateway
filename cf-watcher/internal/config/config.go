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
)

// Config holds all runtime settings for cf-watcher, resolved from
// environment variables. Sensitive values such as CFAPIToken must never be
// logged in full — callers must redact before logging.
type Config struct {
	// Cloudflare API credentials (required — see Validate).
	CFAPIToken  string
	CFAccountID string
	CFTunnelID  string

	// Required only when any container uses cf.dns=true. Empty = not set.
	CFZoneID string

	// Docker integration.
	DockerNetwork string

	// Runtime behaviour.
	LogLevel        string
	DebounceSeconds float64
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
		CFTunnelID:      os.Getenv("CF_TUNNEL_ID"),
		CFZoneID:        os.Getenv("CF_ZONE_ID"),
		DockerNetwork:   stringOrDefault("DOCKER_NETWORK", defaultDockerNetwork),
		LogLevel:        stringOrDefault("LOG_LEVEL", defaultLogLevel),
		DebounceSeconds: floatOrDefault("DEBOUNCE_SECONDS", defaultDebounceSeconds),
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// Validate enforces that all required settings are present and non-empty:
// CF_API_TOKEN, CF_ACCOUNT_ID, CF_TUNNEL_ID. All missing variables are
// reported together in a single error.
func (c *Config) Validate() error {
	var missing []string

	if c.CFAPIToken == "" {
		missing = append(missing, "CF_API_TOKEN")
	}
	if c.CFAccountID == "" {
		missing = append(missing, "CF_ACCOUNT_ID")
	}
	if c.CFTunnelID == "" {
		missing = append(missing, "CF_TUNNEL_ID")
	}

	if len(missing) > 0 {
		return fmt.Errorf("config: missing required environment variable(s): %s", strings.Join(missing, ", "))
	}

	return nil
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
