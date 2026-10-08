// Package tokenfile atomically writes the Cloudflare Tunnel connector token
// to the file cloudflared reads via --token-file.
package tokenfile

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Mode is the permission set on the token file: owner read/write, group
// read. cf-watcher (uid 1000) and cloudflared (uid 65532) run as different
// users, so 0600 would lock cloudflared out. cf-watcher runs with primary
// group 65532 (see cf-watcher/Dockerfile), so every file it creates is
// group-owned by 65532 — cloudflared's group — and 0640 grants cloudflared
// read access without making the token world-readable.
const Mode os.FileMode = 0o640

// Write atomically writes token to path with Mode permissions: the content
// goes to a temp file in the same directory, which is fsynced and then
// renamed over path, so a concurrent reader (cloudflared) never observes a
// partial token. If path already holds exactly token, nothing is written
// and changed is false.
func Write(path, token string) (changed bool, err error) {
	if token == "" {
		return false, errors.New("tokenfile: refusing to write an empty token")
	}

	content := []byte(token)
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, content) {
		return false, nil
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return false, fmt.Errorf("tokenfile: create temp file in %s (is the directory writable by this user?): %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			os.Remove(tmpName)
		}
	}()

	if _, err = tmp.Write(content); err != nil {
		tmp.Close()
		return false, fmt.Errorf("tokenfile: write temp file: %w", err)
	}
	if err = tmp.Chmod(Mode); err != nil {
		tmp.Close()
		return false, fmt.Errorf("tokenfile: chmod temp file: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return false, fmt.Errorf("tokenfile: sync temp file: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return false, fmt.Errorf("tokenfile: close temp file: %w", err)
	}
	if err = os.Rename(tmpName, path); err != nil {
		return false, fmt.Errorf("tokenfile: rename into place: %w", err)
	}
	return true, nil
}
