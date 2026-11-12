//go:build e2e

package harness

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestHarnessGitProbeAuthenticatedRead proves the host Git probe verifies the
// installation CA, authenticates with Basic credentials and asks the served
// smart-HTTP advertisement path.
func TestHarnessGitProbeAuthenticatedRead(t *testing.T) {
	caPath, certificate := testPKI(t)
	var (
		gotPath string
		gotUser string
		gotPass string
		gotRaw  string
	)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotPath = request.URL.Path
		gotRaw = request.URL.RawQuery
		gotUser, gotPass, _ = request.BasicAuth()
		writer.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		_, _ = writer.Write([]byte("001e# service=git-upload-pack\n0000"))
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()

	probe, err := NewGitProbe(server.URL, caPath, Secret("git-token-value"))
	if err != nil {
		t.Fatalf("build Git probe: %v", err)
	}
	body, err := probe.ListRefs(context.Background(), "smoke/run-1")
	if err != nil {
		t.Fatalf("authenticated Git read failed: %v", err)
	}
	if !strings.Contains(string(body), "git-upload-pack") {
		t.Fatalf("Git response body = %q", body)
	}
	if gotUser != "kodama" || gotPass != "git-token-value" {
		t.Fatalf("Git Basic identity = %q/%q", gotUser, gotPass)
	}
	if gotPath != "/smoke/run-1/info/refs" || gotRaw != "service=git-upload-pack" {
		t.Fatalf("Git request = %s?%s", gotPath, gotRaw)
	}
}

// TestHarnessGitProbeRefusesRedirectsAndUntrustedCA proves the host Git reader
// never follows a redirect and never trusts an unrelated CA.
func TestHarnessGitProbeRefusesRedirectsAndUntrustedCA(t *testing.T) {
	caPath, certificate := testPKI(t)
	otherCA, _ := testPKI(t)
	var followed atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/elsewhere" {
			followed.Add(1)
			writer.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(writer, request, "/elsewhere", http.StatusFound)
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()

	probe, err := NewGitProbe(server.URL, caPath, Secret("git-token-value"))
	if err != nil {
		t.Fatalf("build Git probe: %v", err)
	}
	if _, err := probe.ListRefs(context.Background(), "admin"); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("Git redirect error = %v", err)
	}
	if followed.Load() != 0 {
		t.Fatal("the Git probe followed a redirect")
	}

	untrusted, err := NewGitProbe(server.URL, otherCA, Secret("git-token-value"))
	if err != nil {
		t.Fatalf("build untrusted-CA Git probe: %v", err)
	}
	if _, err := untrusted.ListRefs(context.Background(), "admin"); err == nil {
		t.Fatal("the Git probe accepted an unrelated CA")
	}
}
