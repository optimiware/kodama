package production

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func with(c Config, edit func(*Config)) Config {
	res := map[string]string{}
	for k, v := range c.Resources {
		res[k] = v
	}
	c.Resources = res
	edit(&c)
	return c
}

func Test_config_selection(t *testing.T) {
	for _, tc := range []struct {
		provider string
		want     error
	}{
		{"", ErrProviderRequired},
		{"keyfile", ErrUnsupportedProvider},
		{"keyfile-dev", ErrUnsupportedProvider},
		{"ephemeral", ErrUnsupportedProvider},
		{"random", ErrUnsupportedProvider},
		{"azure", ErrUnsupportedProvider},
		{"AWS", ErrUnsupportedProvider},
		{" aws", ErrUnsupportedProvider},
		{"aws,gcp", ErrUnsupportedProvider},
	} {
		_, err := with(awsConfig(), func(c *Config) { c.Provider = tc.provider }).validate()
		if !errors.Is(err, tc.want) {
			t.Errorf("provider %q: %v", tc.provider, err)
		}
	}
	if _, err := (Config{}).validate(); !errors.Is(err, ErrProviderRequired) {
		t.Fatalf("empty config: %v", err)
	}
	// The echoed selection is bounded and printable.
	_, err := with(awsConfig(), func(c *Config) { c.Provider = "x\n" + strings.Repeat("y", 500) }).validate()
	if err == nil || strings.ContainsAny(err.Error(), "\n") || len(err.Error()) > 200 {
		t.Fatalf("echo not sanitized: %q", err)
	}
}

func Test_config_resources(t *testing.T) {
	gcp := func(edit func(*Config)) Config {
		return with(Config{Provider: GCP, KEKID: "k", Current: "v2", Resources: map[string]string{"v1": gcpKeyA, "v2": gcpKeyB}}, edit)
	}
	scw := func(edit func(*Config)) Config {
		return with(Config{Provider: Scaleway, KEKID: "k", Current: "v2", Resources: map[string]string{"v1": scwKeyA, "v2": "fr-par/" + scwKeyB}}, edit)
	}
	tests := []struct {
		name string
		cfg  Config
		ok   bool
	}{
		{"aws ok", awsConfig(), true},
		{"aws arn", with(awsConfig(), func(c *Config) {
			c.Resources["v2"] = "arn:aws:kms:eu-west-1:123456789012:key/" + awsKeyB
		}), true},
		{"aws multi-region", with(awsConfig(), func(c *Config) { c.Resources["v2"] = "mrk-0123456789abcdef0123456789abcdef" }), true},
		{"aws alias", with(awsConfig(), func(c *Config) { c.Resources["v2"] = "alias/kodama" }), false},
		{"aws alias arn", with(awsConfig(), func(c *Config) { c.Resources["v2"] = "arn:aws:kms:eu-west-1:123456789012:alias/kodama" }), false},
		{"aws garbage", with(awsConfig(), func(c *Config) { c.Resources["v2"] = "not a key" }), false},
		{"aws other cloud's resource", with(awsConfig(), func(c *Config) { c.Resources["v2"] = gcpKeyB }), false},
		{"aws empty resource", with(awsConfig(), func(c *Config) { c.Resources["v2"] = "" }), false},
		{"gcp ok", gcp(func(*Config) {}), true},
		{"gcp key version path", gcp(func(c *Config) { c.Resources["v2"] += "/cryptoKeyVersions/1" }), false},
		{"gcp keyring only", gcp(func(c *Config) { c.Resources["v2"] = "projects/p/locations/l/keyRings/r" }), false},
		{"gcp uuid", gcp(func(c *Config) { c.Resources["v2"] = awsKeyB }), false},
		{"gcp resources differing only in case", gcp(func(c *Config) {
			c.Resources["v1"] = "projects/p/locations/l/keyRings/r/cryptoKeys/Key"
			c.Resources["v2"] = "projects/p/locations/l/keyRings/r/cryptoKeys/key"
		}), true},
		{"gcp identical resources", gcp(func(c *Config) { c.Resources["v1"] = c.Resources["v2"] }), false},
		{"scaleway ok", scw(func(*Config) {}), true},
		{"scaleway other region", scw(func(c *Config) { c.Resources["v2"] = "nl-ams/" + scwKeyB }), false},
		{"scaleway garbage", scw(func(c *Config) { c.Resources["v2"] = "fr-par/x" }), false},
		{"duplicate resource", with(awsConfig(), func(c *Config) { c.Resources["v1"] = awsKeyB }), false},
		{"scaleway qualified duplicate", scw(func(c *Config) { c.Resources["v1"] = "fr-par/" + scwKeyB }), false},
		{"scaleway case-folded duplicate", scw(func(c *Config) { c.Resources["v1"] = strings.ToUpper(scwKeyB) }), false},
		{"current unmapped", with(awsConfig(), func(c *Config) { c.Current = "v3" }), false},
		{"no resources", with(awsConfig(), func(c *Config) { c.Resources = nil }), false},
		{"empty kek id", with(awsConfig(), func(c *Config) { c.KEKID = "" }), false},
		{"long kek id", with(awsConfig(), func(c *Config) { c.KEKID = strings.Repeat("k", 257) }), false},
		{"empty current", with(awsConfig(), func(c *Config) { c.Current = "" }), false},
		{"empty label", with(awsConfig(), func(c *Config) { c.Resources[""] = awsKeyA }), false},
		{"control char label", with(awsConfig(), func(c *Config) { c.Resources["v\x00"] = awsKeyA }), false},
		{"negative probe timeout", with(awsConfig(), func(c *Config) { c.ProbeTimeout = -time.Second }), false},
		{"huge probe timeout", with(awsConfig(), func(c *Config) { c.ProbeTimeout = time.Hour }), false},
	}
	for _, tc := range tests {
		_, err := tc.cfg.validate()
		if tc.ok != (err == nil) {
			t.Errorf("%s: err=%v", tc.name, err)
		}
		if err != nil && !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("%s: not ErrInvalidConfig: %v", tc.name, err)
		}
	}
	many := with(awsConfig(), func(c *Config) {
		for i := 0; i < 70; i++ {
			c.Resources[strings.Repeat("v", 1)+string(rune('A'+i%26))+string(rune('a'+i/26))] = "mrk-" + strings.Repeat(string("0123456789abcdef"[i%16]), 16) + strings.Repeat(string("0123456789abcdef"[i/16]), 16)
		}
	})
	if _, err := many.validate(); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("too many versions: %v", err)
	}
}

func Test_config_normalization_and_immutability(t *testing.T) {
	cfg := Config{Provider: Scaleway, KEKID: "k", Current: "v2", Resources: map[string]string{"v1": scwKeyA, "v2": "fr-par/" + scwKeyB}}
	p, err := cfg.validate()
	if err != nil {
		t.Fatal(err)
	}
	if p.current.id != scwKeyB || p.current.keyRef != "fr-par/"+scwKeyB || p.retained[0].keyRef != "fr-par/"+scwKeyA {
		t.Fatalf("scaleway normalization %+v", p)
	}
	bridge := newBridge(p, newFake(Scaleway, p.format, p.current.keyRef))
	// Later changes to the caller's map never affect a constructed adapter.
	cfg.Resources["v2"] = scwKeyA
	cfg.Resources["v3"] = "x"
	delete(cfg.Resources, "v1")
	if len(bridge.resources) != 2 || bridge.resources["v2"].keyRef != "fr-par/"+scwKeyB || bridge.CurrentRef().Version != "v2" {
		t.Fatal("configuration snapshot is not immutable")
	}
	if p.timeout != DefaultProbeTimeout {
		t.Fatalf("default timeout %s", p.timeout)
	}
}
