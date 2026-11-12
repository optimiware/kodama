package keys

// White-box transport pins for the AWS KMS client. The decrypt tests in
// aws_kms_test.go observe behavior; these assertions pin the owned client
// boundary that is otherwise unobservable: no proxy function, a fixed request
// timeout, a redirect refusal and preserved caller TLS settings.

import (
	"crypto/tls"
	"net/http"
	"testing"
	"time"
)

func awsTransportTestConfig() AWSKMSConfig {
	return AWSKMSConfig{
		Region:               "eu-west-3",
		RoleARN:              "arn:aws:iam::111122223333:role/kodama-kms",
		WebIdentityTokenFile: "/var/run/secrets/eks.amazonaws.com/serviceaccount/token",
	}
}

func TestNewAWSKMSOwnsPrivateTransport(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://proxy.invalid:3128")
	t.Setenv("HTTP_PROXY", "http://proxy.invalid:3128")
	client, err := NewAWSKMS(awsTransportTestConfig())
	if err != nil {
		t.Fatalf("NewAWSKMS: %v", err)
	}
	transport, ok := client.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client transport = %T; want *http.Transport", client.client.Transport)
	}
	if transport == http.DefaultTransport {
		t.Fatal("client reuses the shared http.DefaultTransport")
	}
	if transport.Proxy != nil {
		t.Fatal("client transport has a proxy function; want nil so environment proxies never route the token or a signed request")
	}
	if transport.TLSClientConfig != nil && transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("client transport disables TLS verification")
	}
	if client.client.Timeout != 30*time.Second {
		t.Fatalf("client timeout = %v; want the literal 30s", client.client.Timeout)
	}
	// Pin the literals, not the constants they are derived from: a mutant that
	// shortens or lengthens a constant must fail.
	if transport.TLSHandshakeTimeout != 10*time.Second {
		t.Fatalf("TLS handshake timeout = %v; want the literal 10s", transport.TLSHandshakeTimeout)
	}
	if transport.IdleConnTimeout != 90*time.Second {
		t.Fatalf("idle connection timeout = %v; want the literal 90s", transport.IdleConnTimeout)
	}
	if transport.ExpectContinueTimeout != time.Second {
		t.Fatalf("expect-continue timeout = %v; want the literal 1s", transport.ExpectContinueTimeout)
	}
	if transport.DialContext == nil {
		t.Fatal("transport has no explicit dial function")
	}
	if client.client.CheckRedirect == nil {
		t.Fatal("client has no redirect policy")
	}
	if err := client.client.CheckRedirect(nil, nil); err != http.ErrUseLastResponse {
		t.Fatalf("redirect policy = %v; want http.ErrUseLastResponse", err)
	}

	// Each client owns its own transport: two clients never share connections.
	other, err := NewAWSKMS(awsTransportTestConfig())
	if err != nil {
		t.Fatalf("NewAWSKMS: %v", err)
	}
	if other.client.Transport == client.client.Transport {
		t.Fatal("two clients share one transport")
	}
}

func TestNewAWSKMSPreservesCallerTLSWithoutProxyOrMutation(t *testing.T) {
	callerTransport := &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		Proxy:           http.ProxyFromEnvironment,
	}
	redirected := false
	caller := &http.Client{
		Transport: callerTransport,
		Timeout:   time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			redirected = true
			return nil
		},
	}

	config := awsTransportTestConfig()
	config.HTTPClient = caller
	client, err := NewAWSKMS(config)
	if err != nil {
		t.Fatalf("NewAWSKMS: %v", err)
	}
	if client.client == caller {
		t.Fatal("client reused the caller's http.Client")
	}
	owned, ok := client.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client transport = %T; want *http.Transport", client.client.Transport)
	}
	if owned == callerTransport {
		t.Fatal("client reused the caller's transport instead of a private clone")
	}
	if owned.Proxy != nil {
		t.Fatal("private transport kept a proxy function")
	}
	if owned.TLSClientConfig == nil || owned.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Fatal("private transport did not preserve the caller's explicit TLS configuration")
	}
	if callerTransport.Proxy == nil {
		t.Fatal("constructor mutated the caller's transport")
	}
	if client.client.Timeout != 30*time.Second {
		t.Fatalf("client timeout = %v; want the literal 30s", client.client.Timeout)
	}
	// The caller's permissive redirect policy is replaced, and the caller's
	// client keeps its own.
	if err := client.client.CheckRedirect(nil, nil); err != http.ErrUseLastResponse || redirected {
		t.Fatalf("redirect policy = %v (caller policy consulted: %v); want http.ErrUseLastResponse", err, redirected)
	}
	if caller.Timeout != time.Minute || caller.CheckRedirect == nil || caller.Transport != callerTransport {
		t.Fatal("constructor mutated the caller's client")
	}
}
