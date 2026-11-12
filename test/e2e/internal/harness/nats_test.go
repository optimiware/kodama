//go:build e2e

package harness

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeBrokerToken writes one private broker token file.
func writeBrokerToken(t *testing.T, contents string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "broker-token")
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatalf("write broker token file: %v", err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod broker token file: %v", err)
	}
	return path
}

// TestHarnessLiveBrokerInputsAreOptionalAndValidated pins the TLS broker inputs:
// an absent value stays empty (a named skip outside a required run), an
// explicit token file must be private, and the existing admin token file is
// accepted instead of a raw environment token.
func TestHarnessLiveBrokerInputsAreOptionalAndValidated(t *testing.T) {
	unconfigured, err := LoadConfig(func(string) string { return "" })
	if err != nil {
		t.Fatalf("an unconfigured broker input must not be malformed: %v", err)
	}
	if unconfigured.NATSCAFile != "" || unconfigured.NATSTokenFile != "" || unconfigured.NATSTLSServerName != "" {
		t.Fatalf("unconfigured broker inputs = %q/%q/%q; want empty",
			unconfigured.NATSCAFile, unconfigured.NATSTokenFile, unconfigured.NATSTLSServerName)
	}

	caPath, _ := testPKI(t)
	tokenPath := writeBrokerToken(t, "broker-token-value\n", 0o600)
	env := validConfigEnv(t)
	env[string(InputNATSURL)] = "tls://127.0.0.1:14222"
	env[string(InputNATSCAFile)] = caPath
	env[string(InputNATSTokenFile)] = tokenPath
	env[string(InputNATSTLSServerName)] = "nats.kodama-system.svc.cluster.local"
	config, err := LoadConfig(envGetter(env))
	if err != nil {
		t.Fatalf("valid broker inputs were refused: %v", err)
	}
	if config.NATSCAFile != caPath || config.NATSTokenFile != tokenPath ||
		config.NATSTLSServerName != "nats.kodama-system.svc.cluster.local" {
		t.Fatalf("loaded broker inputs = %q/%q/%q", config.NATSCAFile, config.NATSTokenFile, config.NATSTLSServerName)
	}
	if missing := config.Missing(InputNATSURL, InputNATSCAFile, InputNATSTokenFile, InputNATSTLSServerName); !missing.Empty() {
		t.Fatalf("configured broker inputs still report missing: %s", missing)
	}
	token, err := config.LiveBrokerToken()
	if err != nil {
		t.Fatalf("read the broker token: %v", err)
	}
	if token.Reveal() != "broker-token-value" {
		t.Fatal("the broker token file was not read and trimmed")
	}
	for _, rendered := range []string{config.String()} {
		if strings.Contains(rendered, token.Reveal()) {
			t.Fatalf("config rendering leaked the broker token: %s", rendered)
		}
	}

	// The admin token file is acceptable, and the explicit broker token file
	// wins when both are configured.
	fallbackToken := writeBrokerToken(t, "admin-token-value", 0o600)
	fallbackEnv := validConfigEnv(t)
	delete(fallbackEnv, string(InputToken))
	fallbackEnv[string(InputAdminTokenFile)] = fallbackToken
	fallbackEnv[string(InputNATSURL)] = "tls://127.0.0.1:14222"
	fallbackEnv[string(InputNATSCAFile)] = caPath
	fallback, err := LoadConfig(envGetter(fallbackEnv))
	if err != nil {
		t.Fatalf("the admin token file was refused as the broker token: %v", err)
	}
	if missing := fallback.Missing(InputNATSTokenFile); !missing.Empty() {
		t.Fatalf("the admin token file did not satisfy the broker token input: %s", missing)
	}
	fallbackTokenValue, err := fallback.LiveBrokerToken()
	if err != nil {
		t.Fatalf("read the admin token as the broker token: %v", err)
	}
	if fallbackTokenValue.Reveal() != "admin-token-value" {
		t.Fatal("the admin token file was not accepted as the broker token")
	}
	fallbackEnv[string(InputNATSTokenFile)] = tokenPath
	explicit, err := LoadConfig(envGetter(fallbackEnv))
	if err != nil {
		t.Fatalf("explicit broker and admin token files were refused: %v", err)
	}
	explicitToken, err := explicit.LiveBrokerToken()
	if err != nil {
		t.Fatalf("read the explicit broker token: %v", err)
	}
	if explicitToken.Reveal() != "broker-token-value" {
		t.Fatal("the explicit broker token file did not win over the admin token file")
	}
}

// TestHarnessLiveBrokerInputsRefuseWrongInput proves a configured-but-invalid
// broker path, file mode or TLS name is a failure that names its input and
// never echoes the token value.
func TestHarnessLiveBrokerInputsRefuseWrongInput(t *testing.T) {
	caPath, _ := testPKI(t)
	tokenPath := writeBrokerToken(t, "broker-token-value", 0o600)
	notCertificate := writeTempFile(t, "not a certificate\n")

	cases := []struct {
		name   string
		mutate func(map[string]string)
		wants  string
	}{
		{
			name: "missing ca file",
			mutate: func(env map[string]string) {
				env[string(InputNATSCAFile)] = caPath + ".missing"
			},
			wants: "cannot read the CA file",
		},
		{
			name: "ca file without a certificate",
			mutate: func(env map[string]string) {
				env[string(InputNATSCAFile)] = notCertificate
			},
			wants: "contains no certificate",
		},
		{
			name: "missing token file",
			mutate: func(env map[string]string) {
				env[string(InputNATSTokenFile)] = tokenPath + ".missing"
			},
			wants: "cannot read the token file",
		},
		{
			name: "token file is not private",
			mutate: func(env map[string]string) {
				env[string(InputNATSTokenFile)] = writeBrokerToken(t, "broker-token-value", 0o644)
			},
			wants: "must be a 0600 file owned by the caller",
		},
		{
			name: "token file is a directory",
			mutate: func(env map[string]string) {
				env[string(InputNATSTokenFile)] = t.TempDir()
			},
			wants: "not a regular file",
		},
		{
			name: "token file is empty",
			mutate: func(env map[string]string) {
				env[string(InputNATSTokenFile)] = writeBrokerToken(t, "", 0o600)
			},
			wants: "is empty",
		},
		{
			name: "tls server name is a host and port",
			mutate: func(env map[string]string) {
				env[string(InputNATSTLSServerName)] = "nats.kodama-system.svc:4222"
			},
			wants: "must be a plain DNS name",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			env := validConfigEnv(t)
			env[string(InputNATSURL)] = "tls://127.0.0.1:14222"
			env[string(InputNATSCAFile)] = caPath
			env[string(InputNATSTokenFile)] = tokenPath
			testCase.mutate(env)
			_, err := LoadConfig(envGetter(env))
			if err == nil {
				t.Fatal("malformed broker input was accepted")
			}
			if !strings.Contains(err.Error(), testCase.wants) {
				t.Fatalf("malformed broker input error %q does not contain %q", err, testCase.wants)
			}
			if strings.Contains(err.Error(), "broker-token-value") {
				t.Fatalf("malformed broker input error leaked the token value: %v", err)
			}
		})
	}
}

// TestHarnessLiveBrokerTLSConfig pins the TLS builder without a broker: a plain
// local URL keeps its unchanged nil configuration, a tls:// URL builds the
// configured roots and name at TLS 1.2, and a missing CA is refused.
func TestHarnessLiveBrokerTLSConfig(t *testing.T) {
	caPath, _ := testPKI(t)

	plain := Config{NATSURL: "nats://127.0.0.1:20222"}
	tlsConfig, err := plain.LiveBrokerTLS()
	if err != nil {
		t.Fatalf("plain broker refused: %v", err)
	}
	if tlsConfig != nil {
		t.Fatal("a plain local broker unexpectedly selected TLS")
	}

	config := Config{
		NATSURL:    "tls://127.0.0.1:14222",
		NATSCAFile: caPath,
	}
	tlsConfig, err = config.LiveBrokerTLS()
	if err != nil {
		t.Fatalf("TLS broker refused: %v", err)
	}
	if tlsConfig == nil {
		t.Fatal("a tls:// broker did not select TLS")
	}
	if tlsConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("broker TLS minimum version = %d; want TLS 1.2", tlsConfig.MinVersion)
	}
	if tlsConfig.ServerName != "127.0.0.1" {
		t.Fatalf("broker TLS server name = %q; want the URL host", tlsConfig.ServerName)
	}
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatalf("read the test CA bundle: %v", err)
	}
	wantRoots := x509.NewCertPool()
	if !wantRoots.AppendCertsFromPEM(caPEM) {
		t.Fatal("the test CA bundle contains no certificate")
	}
	if tlsConfig.RootCAs == nil || !tlsConfig.RootCAs.Equal(wantRoots) {
		t.Fatal("the broker CA bundle was not loaded")
	}

	config.NATSTLSServerName = "nats.kodama-system.svc.cluster.local"
	tlsConfig, err = config.LiveBrokerTLS()
	if err != nil {
		t.Fatalf("TLS broker with an explicit name refused: %v", err)
	}
	if tlsConfig.ServerName != "nats.kodama-system.svc.cluster.local" {
		t.Fatalf("broker TLS server name = %q; want the configured name", tlsConfig.ServerName)
	}

	// A CA input selects TLS even when the scheme is plain.
	config.NATSURL = "nats://127.0.0.1:20222"
	tlsConfig, err = config.LiveBrokerTLS()
	if err != nil {
		t.Fatalf("plain URL with an explicit CA refused: %v", err)
	}
	if tlsConfig == nil {
		t.Fatal("an explicit CA did not select TLS")
	}

	withoutCA := Config{NATSURL: "tls://127.0.0.1:14222"}
	if _, err := withoutCA.LiveBrokerTLS(); err == nil {
		t.Fatal("a tls:// broker without a CA was accepted")
	} else if !strings.Contains(err.Error(), string(InputNATSCAFile)) {
		t.Fatalf("missing-CA error %q does not name %s", err, InputNATSCAFile)
	}
}
