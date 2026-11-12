package factory

import (
	"crypto/tls"
	"net/http"
	"testing"
)

// TestSecurityOptionsAreOptIn proves TLS and OIDC stay explicit opt-in
// selections: the zero Options value is the existing plain-HTTP, static-human
// harness every current caller keeps using, and each option enables exactly its
// own capability.
func TestSecurityOptionsAreOptIn(t *testing.T) {
	var options Options
	if options.TLS {
		t.Fatalf("zero Options enabled TLS")
	}
	if options.OIDC {
		t.Fatalf("zero Options enabled OIDC")
	}

	WithTLS()(&options)
	if !options.TLS {
		t.Fatalf("WithTLS did not enable TLS")
	}
	if options.OIDC {
		t.Fatalf("WithTLS enabled OIDC")
	}

	WithOIDC()(&options)
	if !options.OIDC {
		t.Fatalf("WithOIDC did not enable OIDC")
	}
	if !options.TLS {
		t.Fatalf("WithOIDC cleared the TLS selection")
	}

	var nilOption Option
	options.apply(nilOption)
	if !options.TLS || !options.OIDC {
		t.Fatalf("a nil option changed the security selection")
	}
}

// TestTLSFixtureLeavesDefaultTransportUntouched proves the TLS fixture never
// mutates the process-wide default transport. The harness runs beside other
// lanes and tests, so a shared InsecureSkipVerify or root-pool change is a
// cross-test security defect.
func TestTLSFixtureLeavesDefaultTransportUntouched(t *testing.T) {
	before := http.DefaultTransport
	beforeTransport, ok := before.(*http.Transport)
	if !ok {
		t.Fatalf("http.DefaultTransport = %T; want *http.Transport", before)
	}
	beforeConfig := beforeTransport.TLSClientConfig
	// Simulate an earlier test in the same process having published the lazy
	// default TLSClientConfig. Without it an in-place RootCAs write would be
	// invisible to a pointer comparison; with it the field assertions below
	// catch exactly that mutation.
	if beforeConfig == nil {
		beforeConfig = &tls.Config{}
		beforeTransport.TLSClientConfig = beforeConfig
		t.Cleanup(func() { beforeTransport.TLSClientConfig = nil })
	}

	fixture := newTLSFixture(t, "127.0.0.1")
	_ = fixture.Client()
	_ = fixture.ServerConfig()

	after := http.DefaultTransport
	if after != before {
		t.Fatalf("TLS fixture replaced http.DefaultTransport")
	}
	afterTransport, ok := after.(*http.Transport)
	if !ok {
		t.Fatalf("http.DefaultTransport = %T after fixture construction; want *http.Transport", after)
	}
	if afterTransport.TLSClientConfig != beforeConfig {
		t.Fatalf("TLS fixture mutated the default transport TLS configuration")
	}
	if afterTransport.TLSClientConfig != nil && afterTransport.TLSClientConfig.InsecureSkipVerify {
		t.Fatalf("default transport verifies with InsecureSkipVerify after fixture construction")
	}
	// A pointer comparison alone misses an in-place write into a TLSClientConfig
	// an earlier test already published. The private CA must never appear there,
	// whether the config is the same pointer or a fresh one.
	if afterTransport.TLSClientConfig != nil && afterTransport.TLSClientConfig.RootCAs != nil {
		t.Fatalf("default transport carries a private CA pool after fixture construction")
	}
	// A zero MinVersion is the standard library's own "use the default floor"
	// state, which Go's transport resolves to TLS 1.2 or newer. Only an
	// explicitly configured weaker minimum would be a defect.
	if afterTransport.TLSClientConfig != nil &&
		afterTransport.TLSClientConfig.MinVersion != 0 &&
		afterTransport.TLSClientConfig.MinVersion < tls.VersionTLS12 {
		t.Fatalf("default transport minimum version = %x; want at least TLS 1.2", afterTransport.TLSClientConfig.MinVersion)
	}
}
