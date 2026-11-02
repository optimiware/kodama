package productmcp

import (
	"errors"
	"testing"
)

func Test_unit_productmcp_fails_closed_without_ports(t *testing.T) {
	f := newFixture(t)
	full := Config{Repositories: fakeRepos{f.w}, Projects: fakeProjects{f.w}, WorkItems: fakeItems{f.w}, Provisioning: fakeProv{f.w}, Results: f.res}
	for name, mut := range map[string]func(*Config){
		"repositories": func(c *Config) { c.Repositories = nil },
		"projects":     func(c *Config) { c.Projects = nil },
		"work items":   func(c *Config) { c.WorkItems = nil },
		"provisioning": func(c *Config) { c.Provisioning = nil },
		"results":      func(c *Config) { c.Results = nil },
		"limits":       func(c *Config) { c.Limits = DefaultLimits(); c.Limits.MaxPage = 0 },
		"page order":   func(c *Config) { c.Limits = DefaultLimits(); c.Limits.DefaultPage = c.Limits.MaxPage + 1 },
	} {
		cfg := full
		mut(&cfg)
		if _, err := NewService(cfg); !errors.Is(err, ErrNotConfigured) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := NewService(full); err != nil {
		t.Fatal(err)
	}
	if _, err := NewHandler(HandlerConfig{Service: f.svc, MaxRequestBytes: 1}); err == nil {
		t.Error("a handler without an authenticator")
	}
	if _, err := NewHandler(HandlerConfig{Authenticator: &tokenAuth{}, MaxRequestBytes: 1}); err == nil {
		t.Error("a handler without a service")
	}
}
