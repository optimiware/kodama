package main

// The durable entries' background scan cadence. The development 20 ms default
// is a busy loop against PostgreSQL and the broker, so the durable entries
// default to one second and accept one explicit, bounded interval through the
// same configuration surface as every other server option: flag >
// environment > config file > default.

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServerLoopIntervalPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.json")
	if err := os.WriteFile(path, []byte(`{"loop_interval":"400ms"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("config file", func(t *testing.T) {
		options, code := parseServerOptions([]string{"--config", path}, io.Discard)
		if code != 0 {
			t.Fatalf("parse exit = %d; want 0", code)
		}
		if options.LoopInterval != 400*time.Millisecond {
			t.Fatalf("loop interval = %s; want the config file's 400ms", options.LoopInterval)
		}
	})

	t.Run("environment overrides the file", func(t *testing.T) {
		t.Setenv("KODAMA_LOOP_INTERVAL", "700ms")
		options, code := parseServerOptions([]string{"--config", path}, io.Discard)
		if code != 0 {
			t.Fatalf("parse exit = %d; want 0", code)
		}
		if options.LoopInterval != 700*time.Millisecond {
			t.Fatalf("loop interval = %s; want the environment's 700ms", options.LoopInterval)
		}
	})

	t.Run("flag overrides the environment", func(t *testing.T) {
		t.Setenv("KODAMA_LOOP_INTERVAL", "700ms")
		options, code := parseServerOptions([]string{"--config", path, "--loop-interval", "900ms"}, io.Discard)
		if code != 0 {
			t.Fatalf("parse exit = %d; want 0", code)
		}
		if options.LoopInterval != 900*time.Millisecond {
			t.Fatalf("loop interval = %s; want the flag's 900ms", options.LoopInterval)
		}
	})

	t.Run("unparseable value is refused", func(t *testing.T) {
		t.Setenv("KODAMA_LOOP_INTERVAL", "soon")
		var stderr bytes.Buffer
		if _, code := parseServerOptions(nil, &stderr); code != 2 {
			t.Fatalf("parse exit = %d; want the configuration refusal 2", code)
		}
		if !strings.Contains(stderr.String(), "KODAMA_LOOP_INTERVAL") {
			t.Fatalf("refusal = %q; want the setting name", stderr.String())
		}
	})
}

func TestProductionLoopIntervalDefaultAndBounds(t *testing.T) {
	getenv, kube, options := productionFixture(t)

	build, err := buildProduction(options, getenv, kube)
	if err != nil {
		t.Fatalf("buildProduction: %v", err)
	}
	if build.Config.LoopInterval != time.Second {
		t.Fatalf("default production loop interval = %s; want 1s", build.Config.LoopInterval)
	}

	options.LoopInterval = 250 * time.Millisecond
	build, err = buildProduction(options, getenv, kube)
	if err != nil {
		t.Fatalf("buildProduction with a configured interval: %v", err)
	}
	if build.Config.LoopInterval != 250*time.Millisecond {
		t.Fatalf("configured production loop interval = %s; want 250ms", build.Config.LoopInterval)
	}

	for _, interval := range []time.Duration{99 * time.Millisecond, time.Minute + time.Second, -time.Second} {
		options.LoopInterval = interval
		if _, err := buildProduction(options, getenv, kube); err == nil {
			t.Fatalf("buildProduction accepted loop interval %s; want a bounded refusal", interval)
		}
	}
}

func TestTestProfileLoopIntervalDefaultAndBounds(t *testing.T) {
	fixture := newTestProfileFixture(t)

	build, err := buildTestProfile(fixture.options, fixture.getenv)
	if err != nil {
		t.Fatalf("buildTestProfile: %v", err)
	}
	if build.Config.LoopInterval != time.Second {
		t.Fatalf("default test profile loop interval = %s; want 1s", build.Config.LoopInterval)
	}

	fixture.options.LoopInterval = 500 * time.Millisecond
	build, err = buildTestProfile(fixture.options, fixture.getenv)
	if err != nil {
		t.Fatalf("buildTestProfile with a configured interval: %v", err)
	}
	if build.Config.LoopInterval != 500*time.Millisecond {
		t.Fatalf("configured test profile loop interval = %s; want 500ms", build.Config.LoopInterval)
	}

	for _, interval := range []time.Duration{50 * time.Millisecond, 2 * time.Minute, -time.Second} {
		fixture.options.LoopInterval = interval
		if _, err := buildTestProfile(fixture.options, fixture.getenv); err == nil {
			t.Fatalf("buildTestProfile accepted loop interval %s; want a bounded refusal", interval)
		}
	}
}
