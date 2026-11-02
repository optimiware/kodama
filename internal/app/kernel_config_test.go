package app

import (
	"strings"
	"testing"
	"time"
)

// Test_unit_kernel_config_events: the kernel assembly maps the NATS URL, its token
// and the outbox poll interval into the serve configuration, and refuses a
// configuration that cannot work instead of falling back.
func Test_unit_kernel_config_events(t *testing.T) {
	cfgOf := func(env map[string]string) (string, string, time.Duration, error) {
		cfg, err := kernelConfig(Config{HTTPAddr: ":0"}, func(k string) string { return env[k] })
		return cfg.NATSURL, cfg.NATSToken, cfg.EventInterval, err
	}
	if url, token, every, err := cfgOf(nil); err != nil || url != "" || token != "" || every != 0 {
		t.Fatalf("defaults: %q %q %v %v", url, token, every, err)
	}
	url, token, every, err := cfgOf(map[string]string{"KODAMA_NATS_URL": "nats://nats.internal:4222", "KODAMA_NATS_TOKEN": "s3cret", "KODAMA_EVENT_POLL_INTERVAL": "750ms"})
	if err != nil || url != "nats://nats.internal:4222" || token != "s3cret" || every != 750*time.Millisecond {
		t.Fatalf("mapped: %q %q %v %v", url, token, every, err)
	}
	if url, _, _, err := cfgOf(map[string]string{"KODAMA_NATS_URL": "tls://tok@nats:4222"}); err != nil || url != "tls://tok@nats:4222" {
		t.Fatalf("a token in the URL: %q %v", url, err)
	}
	for name, env := range map[string]map[string]string{
		"not a NATS scheme":      {"KODAMA_NATS_URL": "http://nats:4222"},
		"no host":                {"KODAMA_NATS_URL": "nats://"},
		"not a URL":              {"KODAMA_NATS_URL": "nats nats"},
		"bare host":              {"KODAMA_NATS_URL": "nats.internal:4222"},
		"a path":                 {"KODAMA_NATS_URL": "nats://nats:4222/js"},
		"a token without a URL":  {"KODAMA_NATS_TOKEN": "s3cret"},
		"interval is not a time": {"KODAMA_EVENT_POLL_INTERVAL": "soon"},
		"interval is zero":       {"KODAMA_EVENT_POLL_INTERVAL": "0s"},
		"interval is too long":   {"KODAMA_EVENT_POLL_INTERVAL": "2h"},
		"git interval too long":  {"KODAMA_GIT_POLL_INTERVAL": "2h"},
	} {
		if _, _, _, err := cfgOf(env); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	// The command reports the refusal and does not start.
	var stderr strings.Builder
	code := runKernel(t.Context(), Config{HTTPAddr: ":0"}, func(k string) string {
		return map[string]string{"KODAMA_NATS_URL": "http://nats:4222"}[k]
	}, nil, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "KODAMA_NATS_URL") {
		t.Fatalf("runKernel = %d %q", code, stderr.String())
	}
}
