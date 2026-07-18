package watcher

import (
	"testing"

	"cf-watcher/internal/state"
)

func TestExtractDesiredState(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   *state.Entry
		wantOK bool
	}{
		{
			// SC-01: happy path — hostname + port present, defaults applied.
			name: "happy path with hostname and port only",
			labels: map[string]string{
				"cf.hostname": "app.vantus.site",
				"cf.port":     "8080",
			},
			want: &state.Entry{
				ContainerID:   "c1",
				ContainerName: "app",
				Hostname:      "app.vantus.site",
				Port:          8080,
				Service:       "http",
				DNS:           false,
			},
			wantOK: true,
		},
		{
			name: "all labels explicit",
			labels: map[string]string{
				"cf.hostname": "warden.vantus.site",
				"cf.port":     "80",
				"cf.service":  "https",
				"cf.dns":      "true",
			},
			want: &state.Entry{
				ContainerID:   "c1",
				ContainerName: "app",
				Hostname:      "warden.vantus.site",
				Port:          80,
				Service:       "https",
				DNS:           true,
			},
			wantOK: true,
		},
		{
			// SC-02: no cf.* labels at all — silently ignored.
			name:   "no cf labels — ignored",
			labels: map[string]string{"other.label": "value"},
			want:   nil,
			wantOK: false,
		},
		{
			name:   "nil labels — ignored",
			labels: nil,
			want:   nil,
			wantOK: false,
		},
		{
			name: "missing cf.hostname — ignored",
			labels: map[string]string{
				"cf.port": "8080",
			},
			want:   nil,
			wantOK: false,
		},
		{
			name: "missing cf.port — ignored",
			labels: map[string]string{
				"cf.hostname": "app.vantus.site",
			},
			want:   nil,
			wantOK: false,
		},
		{
			name: "empty cf.hostname — ignored",
			labels: map[string]string{
				"cf.hostname": "   ",
				"cf.port":     "8080",
			},
			want:   nil,
			wantOK: false,
		},
		{
			// SC-09: invalid cf.port value — skip with warning.
			name: "cf.port not a number — skip",
			labels: map[string]string{
				"cf.hostname": "app.vantus.site",
				"cf.port":     "not-a-number",
			},
			want:   nil,
			wantOK: false,
		},
		{
			name: "cf.port zero — out of range — skip",
			labels: map[string]string{
				"cf.hostname": "app.vantus.site",
				"cf.port":     "0",
			},
			want:   nil,
			wantOK: false,
		},
		{
			name: "cf.port above 65535 — out of range — skip",
			labels: map[string]string{
				"cf.hostname": "app.vantus.site",
				"cf.port":     "65536",
			},
			want:   nil,
			wantOK: false,
		},
		{
			name: "cf.service invalid — falls back to http",
			labels: map[string]string{
				"cf.hostname": "app.vantus.site",
				"cf.port":     "8080",
				"cf.service":  "ftp",
			},
			want: &state.Entry{
				ContainerID:   "c1",
				ContainerName: "app",
				Hostname:      "app.vantus.site",
				Port:          8080,
				Service:       "http",
				DNS:           false,
			},
			wantOK: true,
		},
		{
			name: "cf.dns case-insensitive true",
			labels: map[string]string{
				"cf.hostname": "app.vantus.site",
				"cf.port":     "8080",
				"cf.dns":      "TRUE",
			},
			want: &state.Entry{
				ContainerID:   "c1",
				ContainerName: "app",
				Hostname:      "app.vantus.site",
				Port:          8080,
				Service:       "http",
				DNS:           true,
			},
			wantOK: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ExtractDesiredState("c1", "app", tt.labels)

			if ok != tt.wantOK {
				t.Fatalf("ExtractDesiredState() ok = %v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK {
				if got != nil {
					t.Fatalf("ExtractDesiredState() = %+v, want nil", got)
				}
				return
			}
			if *got != *tt.want {
				t.Fatalf("ExtractDesiredState() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
