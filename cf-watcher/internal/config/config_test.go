package config

import (
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
		"CF_ZONE_ID",
		"DOCKER_NETWORK",
		"LOG_LEVEL",
		"DEBOUNCE_SECONDS",
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
			name: "all required present",
			cfg: Config{
				CFAPIToken:  "token",
				CFAccountID: "account",
				CFTunnelID:  "tunnel",
			},
			wantErr: false,
		},
		{
			name:    "all required missing",
			cfg:     Config{},
			wantErr: true,
			wantMsg: []string{"CF_API_TOKEN", "CF_ACCOUNT_ID", "CF_TUNNEL_ID"},
		},
		{
			name: "missing CF_API_TOKEN only",
			cfg: Config{
				CFAccountID: "account",
				CFTunnelID:  "tunnel",
			},
			wantErr: true,
			wantMsg: []string{"CF_API_TOKEN"},
		},
		{
			name: "missing CF_ACCOUNT_ID only",
			cfg: Config{
				CFAPIToken: "token",
				CFTunnelID: "tunnel",
			},
			wantErr: true,
			wantMsg: []string{"CF_ACCOUNT_ID"},
		},
		{
			name: "missing CF_TUNNEL_ID only",
			cfg: Config{
				CFAPIToken:  "token",
				CFAccountID: "account",
			},
			wantErr: true,
			wantMsg: []string{"CF_TUNNEL_ID"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()

			if tt.wantErr && err == nil {
				t.Fatalf("Validate() = nil, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
			for _, substr := range tt.wantMsg {
				if err != nil && !strings.Contains(err.Error(), substr) {
					t.Errorf("Validate() error = %q, want it to contain %q", err.Error(), substr)
				}
			}
		})
	}
}

func TestLoad_MissingRequiredVar(t *testing.T) {
	clearRequiredEnv(t)
	t.Setenv("CF_API_TOKEN", "token")
	t.Setenv("CF_ACCOUNT_ID", "account")
	// CF_TUNNEL_ID intentionally left unset.

	_, err := Load()
	if err == nil {
		t.Fatal("Load() = nil error, want error for missing CF_TUNNEL_ID")
	}
	if !strings.Contains(err.Error(), "CF_TUNNEL_ID") {
		t.Errorf("Load() error = %q, want it to contain %q", err.Error(), "CF_TUNNEL_ID")
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
			t.Setenv("CF_TUNNEL_ID", "tunnel")
			if tt.debounceSecondsEnv != "" {
				t.Setenv("DEBOUNCE_SECONDS", tt.debounceSecondsEnv)
			}

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() error = %v, want nil", err)
			}

			if cfg.DockerNetwork != defaultDockerNetwork {
				t.Errorf("DockerNetwork = %q, want default %q", cfg.DockerNetwork, defaultDockerNetwork)
			}
			if cfg.LogLevel != defaultLogLevel {
				t.Errorf("LogLevel = %q, want default %q", cfg.LogLevel, defaultLogLevel)
			}
			if cfg.DebounceSeconds != tt.wantDebounceSeconds {
				t.Errorf("DebounceSeconds = %v, want %v", cfg.DebounceSeconds, tt.wantDebounceSeconds)
			}
			if cfg.CFZoneID != "" {
				t.Errorf("CFZoneID = %q, want empty (not set)", cfg.CFZoneID)
			}
		})
	}
}

func TestLoad_OptionalOverrides(t *testing.T) {
	clearRequiredEnv(t)
	t.Setenv("CF_API_TOKEN", "token")
	t.Setenv("CF_ACCOUNT_ID", "account")
	t.Setenv("CF_TUNNEL_ID", "tunnel")
	t.Setenv("CF_ZONE_ID", "zone")
	t.Setenv("DOCKER_NETWORK", "custom-network")
	t.Setenv("LOG_LEVEL", "DEBUG")
	t.Setenv("DEBOUNCE_SECONDS", "1.5")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
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
