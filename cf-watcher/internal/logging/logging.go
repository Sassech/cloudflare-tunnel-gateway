// Package logging is a minimal level-aware wrapper around the standard log
// package, so cf-watcher's LOG_LEVEL setting actually filters output
// instead of being loaded and ignored. It intentionally does not replace
// log.Fatalf — CRITICAL/fatal messages must never be filtered, since they
// are the last thing printed before the process exits.
package logging

import (
	"fmt"
	"log"
	"strings"
	"sync/atomic"
)

// Level is a log severity, ordered low (verbose) to high (severe).
type Level int32

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarning
	LevelError
)

// current is the process-wide minimum level that gets printed. Defaults to
// LevelInfo (matching config.defaultLogLevel) and is set once at startup by
// SetLevel. It's an atomic since log calls happen concurrently from the
// live watcher, the sync loop, and HTTP handler goroutines.
var current atomic.Int32

func init() {
	current.Store(int32(LevelInfo))
}

// ParseLevel maps a LOG_LEVEL string (case-insensitive) to a Level. An
// unrecognized value falls back to LevelInfo — matching config.Load's
// existing fail-soft-to-default convention for malformed optional settings,
// rather than rejecting startup over a typo'd log level.
func ParseLevel(s string) Level {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "DEBUG":
		return LevelDebug
	case "INFO":
		return LevelInfo
	case "WARNING", "WARN":
		return LevelWarning
	case "ERROR":
		return LevelError
	default:
		return LevelInfo
	}
}

// SetLevel sets the process-wide minimum level to print. Call once at
// startup, right after config.Load.
func SetLevel(level Level) {
	current.Store(int32(level))
}

func enabled(level Level) bool {
	return int32(level) >= current.Load()
}

// Debugf logs at DEBUG level if the configured level allows it.
func Debugf(format string, args ...any) {
	if enabled(LevelDebug) {
		log.Print("DEBUG: " + fmt.Sprintf(format, args...))
	}
}

// Info logs a plain (non-formatted) message at INFO level.
func Info(msg string) {
	if enabled(LevelInfo) {
		log.Print("INFO: " + msg)
	}
}

// Infof logs a formatted message at INFO level.
func Infof(format string, args ...any) {
	if enabled(LevelInfo) {
		log.Print("INFO: " + fmt.Sprintf(format, args...))
	}
}

// Warningf logs a formatted message at WARNING level.
func Warningf(format string, args ...any) {
	if enabled(LevelWarning) {
		log.Print("WARNING: " + fmt.Sprintf(format, args...))
	}
}

// Error logs a plain (non-formatted) message at ERROR level.
func Error(msg string) {
	if enabled(LevelError) {
		log.Print("ERROR: " + msg)
	}
}

// Errorf logs a formatted message at ERROR level.
func Errorf(format string, args ...any) {
	if enabled(LevelError) {
		log.Print("ERROR: " + fmt.Sprintf(format, args...))
	}
}
