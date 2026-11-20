package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	opsapp "github.com/optimiware/kodema/internal/shell/app/ops"
)

func TestGitCompanionConfigValidate(t *testing.T) {
	validBase := func() GitCompanionConfig {
		return GitCompanionConfig{
			AdminURL:  "http://127.0.0.1:8081",
			TokenFile: "/run/secrets/git-provision-token",
		}
	}

	t.Run("nil config is valid", func(t *testing.T) {
		var cfg *GitCompanionConfig
		if err := cfg.validate(); err != nil {
			t.Fatalf("nil validate() = %v; want nil", err)
		}
	})

	t.Run("valid minimal and full configs", func(t *testing.T) {
		minimal := validBase()
		if err := minimal.validate(); err != nil {
			t.Fatalf("minimal validate() = %v; want nil", err)
		}

		full := validBase()
		full.AdminURL = "https://git-{tenant}.internal:8443/prefix"
		full.PublicGitURL = "https://git.example.com/git/"
		full.ServiceID = "git-companion_1"
		full.TrustedOrigins = []string{"https://127.0.0.1:8443", "https://kodema.example.com/"}
		if err := full.validate(); err != nil {
			t.Fatalf("full validate() = %v; want nil", err)
		}
	})

	cases := []struct {
		name   string
		mutate func(*GitCompanionConfig)
		want   string
	}{
		{
			name:   "empty admin URL",
			mutate: func(c *GitCompanionConfig) { c.AdminURL = "  " },
			want:   "admin URL",
		},
		{
			name:   "admin URL with query",
			mutate: func(c *GitCompanionConfig) { c.AdminURL = "https://git.internal?a=1" },
			want:   "admin URL",
		},
		{
			name:   "admin URL with trailing question mark",
			mutate: func(c *GitCompanionConfig) { c.AdminURL = "https://git.internal?" },
			want:   "admin URL",
		},
		{
			name:   "admin URL with non-http scheme",
			mutate: func(c *GitCompanionConfig) { c.AdminURL = "ftp://git.internal" },
			want:   "http or https",
		},
		{
			name:   "empty token file",
			mutate: func(c *GitCompanionConfig) { c.TokenFile = "   " },
			want:   "provision token file",
		},
		{
			name:   "whitespace-only public git URL",
			mutate: func(c *GitCompanionConfig) { c.PublicGitURL = "   " },
			want:   "public git URL",
		},
		{
			name:   "relative public git URL",
			mutate: func(c *GitCompanionConfig) { c.PublicGitURL = "/git" },
			want:   "public git URL",
		},
		{
			name:   "public git URL with userinfo",
			mutate: func(c *GitCompanionConfig) { c.PublicGitURL = "https://user:pass@git.example.com/git" },
			want:   "public git URL",
		},
		{
			name:   "public git URL with query",
			mutate: func(c *GitCompanionConfig) { c.PublicGitURL = "https://git.example.com/git?x=1" },
			want:   "public git URL",
		},
		{
			name:   "public git URL with fragment",
			mutate: func(c *GitCompanionConfig) { c.PublicGitURL = "https://git.example.com/git#frag" },
			want:   "public git URL",
		},
		{
			name:   "public git URL with non-http scheme",
			mutate: func(c *GitCompanionConfig) { c.PublicGitURL = "ssh://git.example.com/git" },
			want:   "http or https",
		},
		{
			name:   "whitespace-only service ID",
			mutate: func(c *GitCompanionConfig) { c.ServiceID = "   " },
			want:   "service ID",
		},
		{
			name:   "malformed service ID with spaces",
			mutate: func(c *GitCompanionConfig) { c.ServiceID = "git service" },
			want:   "service ID",
		},
		{
			name:   "malformed service ID with slash",
			mutate: func(c *GitCompanionConfig) { c.ServiceID = "git/companion" },
			want:   "service ID",
		},
		{
			name:   "empty trusted origin element",
			mutate: func(c *GitCompanionConfig) { c.TrustedOrigins = []string{""} },
			want:   "trusted origin",
		},
		{
			name:   "whitespace trusted origin element",
			mutate: func(c *GitCompanionConfig) { c.TrustedOrigins = []string{"   "} },
			want:   "trusted origin",
		},
		{
			name:   "non-https trusted origin",
			mutate: func(c *GitCompanionConfig) { c.TrustedOrigins = []string{"http://127.0.0.1:8443"} },
			want:   "trusted origin",
		},
		{
			name:   "trusted origin with query",
			mutate: func(c *GitCompanionConfig) { c.TrustedOrigins = []string{"https://kodema.example.com?foo=bar"} },
			want:   "trusted origin",
		},
		{
			name:   "trusted origin with userinfo",
			mutate: func(c *GitCompanionConfig) { c.TrustedOrigins = []string{"https://user@kodema.example.com"} },
			want:   "trusted origin",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validBase()
			tc.mutate(&cfg)
			err := cfg.validate()
			if err == nil {
				t.Fatalf("validate() = nil; want error containing %q", tc.want)
			}
			if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
				t.Fatalf("validate() err = %v (class=%v); want InvalidInput", err, class)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validate() err = %q; want substring %q", err.Error(), tc.want)
			}
		})
	}
}

func TestHTTPRepositoryProvisionerRefusesRedirects(t *testing.T) {
	var targetHits atomic.Int32
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer redirectTarget.Close()

	companion := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL+"/steal-token", http.StatusFound)
	}))
	defer companion.Close()

	tokenFile := filepath.Join(t.TempDir(), "provision-token")
	if err := os.WriteFile(tokenFile, []byte("secret-provision-token\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	provisioner, err := newHTTPRepositoryProvisioner(Config{
		GitCompanion: &GitCompanionConfig{
			AdminURL:  companion.URL,
			TokenFile: tokenFile,
		},
	})
	if err != nil {
		t.Fatalf("newHTTPRepositoryProvisioner: %v", err)
	}
	if provisioner.client.CheckRedirect == nil {
		t.Fatal("provisioner HTTP client has nil CheckRedirect")
	}
	if err := provisioner.client.CheckRedirect(nil, nil); err != http.ErrUseLastResponse {
		t.Fatalf("CheckRedirect returned %v; want http.ErrUseLastResponse", err)
	}

	_, err = provisioner.ProvisionRepository(context.Background(), opsapp.RepositoryProvisionRequest{
		TenantID:      id.TenantID("tenant-a"),
		ProjectID:     id.ProjectID("project-a"),
		Repository:    "shop/web",
		ResourceID:    "git-shop-web",
		DefaultBranch: "main",
	})
	if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
		t.Fatalf("ProvisionRepository on redirect err = %v; want DependencyUnavailable", err)
	}
	if hits := targetHits.Load(); hits != 0 {
		t.Fatalf("redirect target received %d requests; want 0 (redirects must never be followed)", hits)
	}
}

func TestHTTPRepositoryProvisionerMaps401ToDependencyUnavailable(t *testing.T) {
	companion := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("WWW-Authenticate", `Bearer realm="kodema"`)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"unauthenticated","message":"authentication failed"}`))
	}))
	defer companion.Close()

	tokenFile := filepath.Join(t.TempDir(), "provision-token")
	if err := os.WriteFile(tokenFile, []byte("wrong-token\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	provisioner, err := newHTTPRepositoryProvisioner(Config{
		GitCompanion: &GitCompanionConfig{
			AdminURL:  companion.URL,
			TokenFile: tokenFile,
		},
	})
	if err != nil {
		t.Fatalf("newHTTPRepositoryProvisioner: %v", err)
	}

	_, err = provisioner.ProvisionRepository(context.Background(), opsapp.RepositoryProvisionRequest{
		TenantID:      id.TenantID("tenant-a"),
		ProjectID:     id.ProjectID("project-a"),
		Repository:    "shop/web",
		ResourceID:    "git-shop-web",
		DefaultBranch: "main",
	})
	if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
		t.Fatalf("ProvisionRepository on HTTP 401 err = %v (class=%v); want DependencyUnavailable", err, class)
	}
}
