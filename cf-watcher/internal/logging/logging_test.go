package logging

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

// captureOutput redirects the standard logger's output for the duration of
// fn and returns everything written to it.
func captureOutput(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	orig := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(orig)
		log.SetFlags(origFlags)
	}()

	fn()
	return buf.String()
}

func TestParseLevel(t *testing.T) {
	tests := []struct {
		in   string
		want Level
	}{
		{"DEBUG", LevelDebug},
		{"debug", LevelDebug},
		{"INFO", LevelInfo},
		{"WARNING", LevelWarning},
		{"WARN", LevelWarning},
		{"ERROR", LevelError},
		{"", LevelInfo},
		{"not-a-level", LevelInfo},
		{"  info  ", LevelInfo},
	}
	for _, tt := range tests {
		if got := ParseLevel(tt.in); got != tt.want {
			t.Errorf("ParseLevel(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestSetLevel_FiltersBelowThreshold(t *testing.T) {
	t.Cleanup(func() { SetLevel(LevelInfo) }) // restore default for other tests

	SetLevel(LevelWarning)

	out := captureOutput(t, func() {
		Debugf("debug message")
		Info("info message")
		Warningf("warning message")
		Error("error message")
	})

	if strings.Contains(out, "debug message") {
		t.Errorf("output contains DEBUG line at LevelWarning, want filtered:\n%s", out)
	}
	if strings.Contains(out, "info message") {
		t.Errorf("output contains INFO line at LevelWarning, want filtered:\n%s", out)
	}
	if !strings.Contains(out, "WARNING: warning message") {
		t.Errorf("output missing WARNING line at LevelWarning:\n%s", out)
	}
	if !strings.Contains(out, "ERROR: error message") {
		t.Errorf("output missing ERROR line at LevelWarning:\n%s", out)
	}
}

func TestSetLevel_DebugAllowsEverything(t *testing.T) {
	t.Cleanup(func() { SetLevel(LevelInfo) })

	SetLevel(LevelDebug)

	out := captureOutput(t, func() {
		Debugf("debug message")
		Info("info message")
	})

	if !strings.Contains(out, "DEBUG: debug message") {
		t.Errorf("output missing DEBUG line at LevelDebug:\n%s", out)
	}
	if !strings.Contains(out, "INFO: info message") {
		t.Errorf("output missing INFO line at LevelDebug:\n%s", out)
	}
}

func TestInfof_FormatsArgs(t *testing.T) {
	t.Cleanup(func() { SetLevel(LevelInfo) })
	SetLevel(LevelInfo)

	out := captureOutput(t, func() {
		Infof("hostname=%s port=%d", "app.example.com", 8080)
	})

	want := "INFO: hostname=app.example.com port=8080"
	if !strings.Contains(out, want) {
		t.Errorf("output = %q, want it to contain %q", out, want)
	}
}
