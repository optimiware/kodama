package main

import (
	"maps"
	"strings"
	"testing"

	shellapp "github.com/optimiware/kodama/internal/shell/app"
)

// TestNATSAuthFromEnv pins the deployment contract for per-credential broker
// access: no setting leaves the feature off, a complete pair enables it, and
// half a pair is a configuration refusal rather than a silently weaker broker
// path.
func TestNATSAuthFromEnv(t *testing.T) {
	env := func(values map[string]string) productionEnv {
		return func(name string) string { return values[name] }
	}
	full := map[string]string{
		"KODAMA_NATS_USER":                "shell",
		"KODAMA_NATS_PASSWORD":            "shell-password",
		"KODAMA_NATS_CALLOUT_USER":        "callout",
		"KODAMA_NATS_CALLOUT_PASSWORD":    "callout-password",
		"KODAMA_NATS_CALLOUT_ISSUER_SEED": "SAseed",
		"KODAMA_NATS_CALLOUT_ACCOUNT":     "KODAMA",
	}

	if config, err := natsAuthFromEnv(env(map[string]string{})); err != nil || config != nil {
		t.Fatalf("empty environment = %+v, %v; want nil, nil", config, err)
	}
	config, err := natsAuthFromEnv(env(full))
	if err != nil {
		t.Fatalf("complete environment: %v", err)
	}
	want := shellapp.NATSAuthConfig{
		User:     "shell",
		Password: "shell-password",
		Callout: &shellapp.NATSCalloutConfig{
			User:       "callout",
			Password:   "callout-password",
			IssuerSeed: "SAseed",
			Account:    "KODAMA",
		},
	}
	if config == nil || config.User != want.User || config.Password != want.Password || config.Callout == nil {
		t.Fatalf("complete environment = %+v; want %+v", config, want)
	}
	if *config.Callout != *want.Callout {
		t.Fatalf("callout environment = %+v; want %+v", *config.Callout, *want.Callout)
	}

	cases := []struct {
		name   string
		mutate func(map[string]string)
		want   string
	}{
		{"bypass user without password", func(values map[string]string) { delete(values, "KODAMA_NATS_PASSWORD") }, "KODAMA_NATS_PASSWORD"},
		{"callout without seed", func(values map[string]string) { delete(values, "KODAMA_NATS_CALLOUT_ISSUER_SEED") }, "KODAMA_NATS_CALLOUT_ISSUER_SEED"},
		{"callout without bypass", func(values map[string]string) {
			delete(values, "KODAMA_NATS_USER")
			delete(values, "KODAMA_NATS_PASSWORD")
		}, "KODAMA_NATS_USER"},
	}
	for _, testCase := range cases {
		values := map[string]string{}
		maps.Copy(values, full)
		testCase.mutate(values)
		_, err := natsAuthFromEnv(env(values))
		if err == nil || !strings.Contains(err.Error(), testCase.want) {
			t.Fatalf("%s error = %v; want it to name %s", testCase.name, err, testCase.want)
		}
	}
}
