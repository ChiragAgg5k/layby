package main

import (
	"testing"
	"time"
)

// Polling an SSH-fronted sandbox too aggressively trips the host's SSH rate
// limiter and locks the client out — the DigitalOcean Docker image ships
// `ufw 22/tcp LIMIT`, which rejects an IP after 6 connections in 30 seconds.
// The resulting refusals are indistinguishable from a sandbox that never
// booted, so every driver must declare an interval it can survive.
func TestEveryDriverDeclaresASurvivableReadinessPoll(t *testing.T) {
	const ufwLimitFloor = 5 * time.Second

	for name, driver := range drivers() {
		capabilities := driver.Capabilities()

		if capabilities.ReadinessPollInterval <= 0 {
			t.Errorf("driver %q declares no readiness poll interval", name)
		}
		if capabilities.ReadinessTimeout <= 0 {
			t.Errorf("driver %q declares no readiness timeout", name)
		}
		if capabilities.ReadinessTimeout <= capabilities.ReadinessPollInterval {
			t.Errorf("driver %q would poll at most once before timing out", name)
		}
		// Only providers reached over SSH are subject to a rate limiter.
		if capabilities.InteractiveShell && name != "local" &&
			capabilities.ReadinessPollInterval < ufwLimitFloor {
			t.Errorf("driver %q polls every %s, fast enough to trip an SSH rate limiter",
				name, capabilities.ReadinessPollInterval)
		}
	}
}
