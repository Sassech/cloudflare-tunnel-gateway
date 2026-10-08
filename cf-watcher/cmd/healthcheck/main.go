// Command healthcheck is a minimal binary used by the Docker HEALTHCHECK
// directive to probe cf-watcher's /healthz endpoint. It exists because the
// final distroless image ships no shell, curl, or wget to run a normal
// healthcheck command — see cf-watcher/Dockerfile.
//
// Exit code 0 means healthy, 1 means unhealthy, matching Docker's
// HEALTHCHECK CMD convention.
package main

import (
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	addr := os.Getenv("HEALTH_ADDR")
	if addr == "" {
		addr = ":9090" // must match internal/config's defaultHealthAddr
	}
	if strings.EqualFold(addr, "off") {
		// Health server intentionally disabled — nothing to probe, don't
		// fail the container over a check the operator turned off.
		os.Exit(0)
	}

	target := addr
	if strings.HasPrefix(addr, ":") {
		target = "localhost" + addr
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + target + "/healthz")
	if err != nil {
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		os.Exit(1)
	}
}
