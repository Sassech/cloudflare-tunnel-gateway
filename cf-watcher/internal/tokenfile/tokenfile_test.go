package tokenfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWrite(t *testing.T) {
	tests := []struct {
		name        string
		existing    *string // nil means the file does not exist yet
		token       string
		wantChanged bool
		wantErr     bool
	}{
		{name: "creates a missing file", token: "tok-1", wantChanged: true},
		{name: "rewrites on content change", existing: strPtr("tok-old"), token: "tok-new", wantChanged: true},
		{name: "no-op when content is unchanged", existing: strPtr("tok-1"), token: "tok-1", wantChanged: false},
		{name: "rejects an empty token", token: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "token")
			if tt.existing != nil {
				if err := os.WriteFile(path, []byte(*tt.existing), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var beforeMod os.FileInfo
			if tt.existing != nil {
				beforeMod, _ = os.Stat(path)
			}

			changed, err := Write(path, tt.token)
			if tt.wantErr != (err != nil) {
				t.Fatalf("Write() error = %v, wantErr %v", err, tt.wantErr)
			}
			if changed != tt.wantChanged {
				t.Errorf("Write() changed = %v, want %v", changed, tt.wantChanged)
			}
			if tt.wantErr {
				return
			}

			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile() error = %v", err)
			}
			if string(got) != tt.token {
				t.Errorf("file content = %q, want %q", got, tt.token)
			}

			info, _ := os.Stat(path)
			if tt.wantChanged && info.Mode().Perm() != Mode {
				t.Errorf("file mode = %o, want %o", info.Mode().Perm(), Mode)
			}
			if !tt.wantChanged && !os.SameFile(beforeMod, info) {
				t.Error("unchanged token replaced the file, want it left untouched")
			}

			// No temp files may be left behind.
			entries, _ := os.ReadDir(dir)
			if len(entries) != 1 {
				t.Errorf("dir has %d entries, want only the token file", len(entries))
			}
		})
	}
}

func TestWrite_UnwritableDirFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-subdir", "token")
	if _, err := Write(path, "tok"); err == nil {
		t.Fatal("Write() error = nil, want error for a missing directory")
	}
}

func strPtr(s string) *string { return &s }
