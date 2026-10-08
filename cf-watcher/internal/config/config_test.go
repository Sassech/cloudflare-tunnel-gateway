package config

import (
	"errors"
	"strings"
	"testing"
)

// clearRequiredEnv unsets every environment variable Config reads, so each
// test starts from a clean slate regardless of the host shell's env.
func clearRequiredEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"CF_API_TOKEN",
		"CF_ACCOUNT_ID",
		"CF_TUNNEL_ID",
		"CF_TUNNEL_NAME",
		"TUNNEL_TOKEN_FILE",
		"CF_ZONE_ID",
		"DOCKER_NETWORK",
		"LOG_LEVEL",
		"DEBOUNCE_SECONDS",
		"HEALTH_ADDR",
		"RELOAD_TOKEN",
	} {
		t.Setenv(key, "")
	}
}

func TestConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
		wantMsg []string // substrings that must appear in the error message
	}{
		{
			name: "all required present, no tunnel ID (provision by name)",
			cfg: Config{
				CFAPIToken:      "token",
				CFAccountID:     "account",
				CFTunnelName:    "cf-watcher-vps1",
				TunnelTokenFile: "/run/cloudflared/token",
			},
			wantErr: false,
		},
		{
			name: "explicit tunnel ID is accepted",
			cfg: Config{
				CFAPIToken:      "token",
				CFAccountID:     "account",
				CFTunnelName:    "cf-watcher-vps1",
				CFTunnelID:      "tunnel",
				TunnelTokenFile: "/run/cloudflared/token",
			},
			wantErr: false,
		},
		{
			name:    "all required missing",
			cfg:     Config{},
			wantErr: true,
			wantMsg: []string{"CF_API_TOKEN", "CF_ACCOUNT_ID", "CF_TUNNEL_NAME", "TUNNEL_TOKEN_FILE"},
		},
		{
			name: "missing CF_API_TOKEN only",
			cfg: Config{
				CFAccountID:     "account",
				CFTunnelName:    "name",
				TunnelTokenFile: "/run/cloudflared/token",
			},
			wantErr: true,
			wantMsg: []string{"CF_API_TOKEN"},
		},
		{
			name: "missing CF_ACCOUNT_ID only",
			cfg: Config{
				CFAPIToken:      "token",
				CFTunnelName:    "name",
				TunnelTokenFile: "/run/cloudflared/token",
			},
			wantErr: true,
			wantMsg: []string{"CF_ACCOUNT_ID"},
		},
		{
			name: "blank tunnel name",
			cfg: Config{
				CFAPIToken:      "token",
				CFAccountID:     "account",
				CFTunnelName:    "   ",
				TunnelTokenFile: "/run/cloudflared/token",
			},
			wantErr: true,
			wantMsg: []string{"CF_TUNNEL_NAME"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			assertValidateError(t, err, tt.wantErr, tt.wantMsg)
		})
	}
}

// assertValidateError asserts that Validate returned the expected error
// presence, and that each wantMsg substring appears in the error text when
// an error was expected.
func assertValidateError(t *testing.T, err error, wantErr bool, wantMsg []string) {
	t.Helper()
	if wantErr && err == nil {
		t.Fatalf("Validate() = nil, want error")
	}
	if !wantErr && err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
	for _, substr := range wantMsg {
		if err != nil && !strings.Contains(err.Error(), substr) {
			t.Errorf("Validate() error = %q, want it to contain %q", err.Error(), substr)
		}
	}
}

func TestLoad_MissingRequiredVar(t *testing.T) {
	clearRequiredEnv(t)
	t.Setenv("CF_API_TOKEN", "token")
	// CF_ACCOUNT_ID intentionally left unset.

	_, err := Load()
	if err == nil {
		t.Fatal("Load() = nil error, want error for missing CF_ACCOUNT_ID")
	}
	if !strings.Contains(err.Error(), "CF_ACCOUNT_ID") {
		t.Errorf("Load() error = %q, want it to contain %q", err.Error(), "CF_ACCOUNT_ID")
	}
}

// stubHostname replaces the package hostname lookup for the duration of t.
func stubHostname(t *testing.T, name string, err error) {
	t.Helper()
	orig := hostname
	hostname = func() (string, error) { return name, err }
	t.Cleanup(func() { hostname = orig })
}

func TestLoad_TunnelName(t *testing.T) {
	tests := []struct {
		name        string
		envName     string // "" means leave unset
		host        string
		hostErr     error
		wantName    string
		wantErr     bool
		wantErrText string
	}{
		{
			name:     "defaults to cf-watcher-<hostname>",
			host:     "vps-eu-1",
			wantName: "cf-watcher-vps-eu-1",
		},
		{
			name:     "explicit CF_TUNNEL_NAME wins over hostname",
			envName:  "  edge-01  ",
			host:     "vps-eu-1",
			wantName: "edge-01",
		},
		{
			name:        "hostname lookup failure without CF_TUNNEL_NAME is an error",
			hostErr:     errors.New("no hostname"),
			wantErr:     true,
			wantErrText: "CF_TUNNEL_NAME",
		},
		{
			name:     "hostname lookup failure is irrelevant when CF_TUNNEL_NAME is set",
			envName:  "edge-02",
			hostErr:  errors.New("no hostname"),
			wantName: "edge-02",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearRequiredEnv(t)
			t.Setenv("CF_API_TOKEN", "token")
			t.Setenv("CF_ACCOUNT_ID", "account")
			if tt.envName != "" {
				t.Setenv("CF_TUNNEL_NAME", tt.envName)
			}
			stubHostname(t, tt.host, tt.hostErr)

			cfg, err := Load()
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrText) {
					t.Fatalf("Load() error = %v, want error containing %q", err, tt.wantErrText)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v, want nil", err)
			}
			if cfg.CFTunnelName != tt.wantName {
				t.Errorf("CFTunnelName = %q, want %q", cfg.CFTunnelName, tt.wantName)
			}
		})
	}
}

func TestLoad_Defaults(t *testing.T) {
	tests := []struct {
		name                string
		debounceSecondsEnv  string // "" means leave unset
		wantDebounceSeconds float64
	}{
		{
			name:                "optional vars absent",
			debounceSecondsEnv:  "",
			wantDebounceSeconds: defaultDebounceSeconds,
		},
		{
			name:                "invalid DEBOUNCE_SECONDS falls back to default",
			debounceSecondsEnv:  "not-a-number",
			wantDebounceSeconds: defaultDebounceSeconds,
		},
		{
			name:                "valid DEBOUNCE_SECONDS overrides default",
			debounceSecondsEnv:  "5.5",
			wantDebounceSeconds: 5.5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearRequiredEnv(t)
			t.Setenv("CF_API_TOKEN", "token")
			t.Setenv("CF_ACCOUNT_ID", "account")
			stubHostname(t, "host", nil)
			if tt.debounceSecondsEnv != "" {
				t.Setenv("DEBOUNCE_SECONDS", tt.debounceSecondsEnv)
			}

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() error = %v, want nil", err)
			}

			assertDefaultConfig(t, cfg, tt.wantDebounceSeconds)
		})
	}
}

// assertDefaultConfig asserts that Load returned the expected defaults for
// every optional variable, including the debounce override under test.
func assertDefaultConfig(t *testing.T, cfg *Config, wantDebounceSeconds float64) {
	t.Helper()
	if cfg.DockerNetwork != defaultDockerNetwork {
		t.Errorf("DockerNetwork = %q, want default %q", cfg.DockerNetwork, defaultDockerNetwork)
	}
	if cfg.LogLevel != defaultLogLevel {
		t.Errorf("LogLevel = %q, want default %q", cfg.LogLevel, defaultLogLevel)
	}
	if cfg.DebounceSeconds != wantDebounceSeconds {
		t.Errorf("DebounceSeconds = %v, want %v", cfg.DebounceSeconds, wantDebounceSeconds)
	}
	if cfg.CFZoneID != "" {
		t.Errorf("CFZoneID = %q, want empty (not set)", cfg.CFZoneID)
	}
	if cfg.HealthAddr != defaultHealthAddr {
		t.Errorf("HealthAddr = %q, want default %q", cfg.HealthAddr, defaultHealthAddr)
	}
	if cfg.ReloadToken != "" {
		t.Errorf("ReloadToken = %q, want empty (disabled by default)", cfg.ReloadToken)
	}
	if cfg.CFTunnelID != "" {
		t.Errorf("CFTunnelID = %q, want empty (provision by name)", cfg.CFTunnelID)
	}
	if cfg.CFTunnelName != "cf-watcher-host" {
		t.Errorf("CFTunnelName = %q, want %q", cfg.CFTunnelName, "cf-watcher-host")
	}
	if cfg.TunnelTokenFile != defaultTunnelTokenFile {
		t.Errorf("TunnelTokenFile = %q, want default %q", cfg.TunnelTokenFile, defaultTunnelTokenFile)
	}
}

func TestLoad_OptionalOverrides(t *testing.T) {
	clearRequiredEnv(t)
	t.Setenv("CF_API_TOKEN", "token")
	t.Setenv("CF_ACCOUNT_ID", "account")
	t.Setenv("CF_TUNNEL_ID", "tunnel")
	t.Setenv("CF_TUNNEL_NAME", "edge-01")
	t.Setenv("TUNNEL_TOKEN_FILE", "/tmp/token")
	t.Setenv("CF_ZONE_ID", "zone")
	t.Setenv("DOCKER_NETWORK", "custom-network")
	t.Setenv("LOG_LEVEL", "DEBUG")
	t.Setenv("DEBOUNCE_SECONDS", "1.5")
	t.Setenv("HEALTH_ADDR", ":9999")
	t.Setenv("RELOAD_TOKEN", "shh-secret")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}

	if cfg.CFTunnelID != "tunnel" {
		t.Errorf("CFTunnelID = %q, want %q", cfg.CFTunnelID, "tunnel")
	}
	if cfg.CFTunnelName != "edge-01" {
		t.Errorf("CFTunnelName = %q, want %q", cfg.CFTunnelName, "edge-01")
	}
	if cfg.TunnelTokenFile != "/tmp/token" {
		t.Errorf("TunnelTokenFile = %q, want %q", cfg.TunnelTokenFile, "/tmp/token")
	}
	if cfg.HealthAddr != ":9999" {
		t.Errorf("HealthAddr = %q, want %q", cfg.HealthAddr, ":9999")
	}
	if cfg.ReloadToken != "shh-secret" {
		t.Errorf("ReloadToken = %q, want %q", cfg.ReloadToken, "shh-secret")
	}
	if cfg.CFZoneID != "zone" {
		t.Errorf("CFZoneID = %q, want %q", cfg.CFZoneID, "zone")
	}
	if cfg.DockerNetwork != "custom-network" {
		t.Errorf("DockerNetwork = %q, want %q", cfg.DockerNetwork, "custom-network")
	}
	if cfg.LogLevel != "DEBUG" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "DEBUG")
	}
	if cfg.DebounceSeconds != 1.5 {
		t.Errorf("DebounceSeconds = %v, want %v", cfg.DebounceSeconds, 1.5)
	}
}
