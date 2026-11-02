package gitworker_test

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kube/gitworker"
)

// Test_unit_gitworker_no_platform_credentials (T-129,
// unit_gitworker_no_platform_credentials, GR-03, IT-17, IT-A13): the worker holds
// no Kubernetes API token and references no Secret by environment; the only
// credential material it receives is its issued, tenant-bound identity token and
// public verification material; a signing key or another tenant's identity is
// refused before anything is created.
func Test_unit_gitworker_no_platform_credentials(t *testing.T) {
	o := mustRender(t, acme, testSpec())
	pod := o.Deployment.Spec.Template.Spec
	c := pod.Containers[0]

	t.Run("no Kubernetes API identity", func(t *testing.T) {
		if o.ServiceAccount.AutomountServiceAccountToken == nil || *o.ServiceAccount.AutomountServiceAccountToken {
			t.Fatal("the service account mounts its token")
		}
		if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
			t.Fatal("the Pod mounts a service account token")
		}
		if pod.ServiceAccountName != gitworker.ServiceAccountName {
			t.Fatalf("service account %q: the worker must not run as the namespace default", pod.ServiceAccountName)
		}
		if pod.EnableServiceLinks == nil || *pod.EnableServiceLinks {
			t.Fatal("service links expose cluster service addresses in the environment")
		}
	})

	t.Run("the whole environment is configuration, never a credential", func(t *testing.T) {
		var names []string
		for _, e := range c.Env {
			names = append(names, e.Name)
			if e.ValueFrom != nil && (e.ValueFrom.FieldRef == nil || e.ValueFrom.FieldRef.FieldPath != "metadata.uid") {
				t.Fatalf("%s is read from something other than the Pod's own UID", e.Name)
			}
			lower := strings.ToLower(e.Name + "=" + e.Value)
			for _, bad := range []string{"password", "postgres", "database", "nats", "broker", "kms", "signing", "secret", "api_key", "kubeconfig", "bearer"} {
				if strings.Contains(lower, bad) {
					t.Fatalf("%s names %q", e.Name, bad)
				}
			}
		}
		sort.Strings(names)
		want := []string{"HOME", "KODAMA_GIT_CA_FILE", "KODAMA_GIT_CONTROL_URL", "KODAMA_GIT_DATA_ROOT", "KODAMA_GIT_IDENTITY_TOKEN_FILE",
			"KODAMA_GIT_LISTEN_ADDR", "KODAMA_GIT_PUBLIC_KEYS_FILE", "KODAMA_GIT_TENANT_ID", "KODAMA_GIT_WORKER_INSTANCE", "TMPDIR"}
		if strings.Join(names, ",") != strings.Join(want, ",") {
			t.Fatalf("environment %v", names)
		}
		raw := marshal(t, o.Deployment)
		for _, bad := range []string{"envFrom", "secretKeyRef", "configMapKeyRef", "hostPath", "projected", "serviceAccountToken"} {
			if strings.Contains(raw, bad) {
				t.Fatalf("the Deployment contains %q", bad)
			}
		}
	})

	t.Run("volumes are the tenant's volume, scratch and the identity files", func(t *testing.T) {
		names := map[string]bool{}
		for _, v := range pod.Volumes {
			names[v.Name] = true
			switch v.Name {
			case "data":
				if v.PersistentVolumeClaim == nil || v.PersistentVolumeClaim.ClaimName != gitworker.ClaimName {
					t.Fatalf("data volume: %+v", v)
				}
			case "tmp":
				if v.EmptyDir == nil || v.EmptyDir.Medium != "" || v.EmptyDir.SizeLimit == "" {
					t.Fatalf("tmp volume: %+v", v)
				}
			case "identity":
				if v.Secret == nil || v.Secret.SecretName != gitworker.SecretName || len(v.Secret.Items) != 3 || v.Secret.Optional == nil || *v.Secret.Optional {
					t.Fatalf("identity volume: %+v", v)
				}
				for _, it := range v.Secret.Items {
					if it.Key != it.Path || (it.Key != gitworker.KeyToken && it.Key != gitworker.KeyPublicKeys && it.Key != gitworker.KeyCA) {
						t.Fatalf("projected key %s", it.Key)
					}
				}
			default:
				t.Fatalf("unexpected volume %s", v.Name)
			}
		}
		if len(names) != 3 {
			t.Fatalf("volumes %v", names)
		}
		for _, m := range c.VolumeMounts {
			if m.Name == "identity" && !m.ReadOnly {
				t.Fatal("the identity files are writable")
			}
		}
	})

	t.Run("a restricted container", func(t *testing.T) {
		sc := c.SecurityContext
		if sc == nil || *sc.Privileged || *sc.AllowPrivilegeEscalation || !*sc.ReadOnlyRootFilesystem || !*sc.RunAsNonRoot || *sc.RunAsUser == 0 ||
			sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" || len(sc.Capabilities.Add) != 0 ||
			sc.SeccompProfile == nil || sc.SeccompProfile.Type != "RuntimeDefault" {
			t.Fatalf("container security: %+v", sc)
		}
		ps := pod.SecurityContext
		if ps == nil || !*ps.RunAsNonRoot || ps.SeccompProfile == nil || ps.SeccompProfile.Type != "RuntimeDefault" {
			t.Fatalf("pod security: %+v", ps)
		}
		if pod.HostNetwork || pod.HostPID || pod.HostIPC {
			t.Fatal("host namespaces")
		}
		if c.ReadinessProbe == nil || c.LivenessProbe == nil || c.ReadinessProbe.HTTPGet.Path != "/healthz" || c.LivenessProbe.HTTPGet.Path != "/healthz" || c.ReadinessProbe.HTTPGet.Scheme != "HTTPS" {
			t.Fatalf("probes: %+v %+v", c.ReadinessProbe, c.LivenessProbe)
		}
		if c.Resources == nil || len(c.Resources.Limits) != 3 || len(c.Resources.Requests) != 3 {
			t.Fatalf("resources: %+v", c.Resources)
		}
	})

	t.Run("the Secret holds the issued identity and public material only", func(t *testing.T) {
		e := newEnv(t)
		e.reconcile(acme)
		s, ok := e.f.secret(nsOf(acme))
		if !ok {
			t.Fatal("no identity Secret")
		}
		keys := []string{}
		for k := range s.Data {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if strings.Join(keys, ",") != "ca.crt,public-keys,token" {
			t.Fatalf("keys %v", keys)
		}
		if string(s.Data["token"]) != "worker-token.t_acme" || s.Immutable != nil {
			t.Fatalf("token %q immutable %v", s.Data["token"], s.Immutable)
		}
	})

	jwkPublic := []byte(`{"keys":[{"kty":"EC","crv":"P-256","x":"AAAA","y":"BBBB"}]}`)
	jwkPrivate := []byte(`{"keys":[{"kty":"EC","crv":"P-256","x":"AAAA","y":"BBBB","d":"CCCC"}]}`)
	jwkRSA := []byte(`{"keys":[{"kty":"RSA","n":"AA","e":"AQAB","d":"BB","p":"CC","q":"DD","dp":"EE","dq":"FF","qi":"GG"}]}`)
	jwkOct := []byte(`{"keys":[{"kty":"oct","k":"c2VjcmV0"}]}`)
	refused := map[string]func(domain.TenantID, *gitworker.Material){
		"another tenant's identity": func(_ domain.TenantID, m *gitworker.Material) { m.TenantID = "t_other" },
		"an expired token":          func(_ domain.TenantID, m *gitworker.Material) { m.ExpiresAt = m.ExpiresAt.Add(-time.Hour) },
		"a token about to expire": func(_ domain.TenantID, m *gitworker.Material) {
			m.ExpiresAt = time.Date(2026, 10, 30, 12, 0, 30, 0, time.UTC)
		},
		"no token":                         func(_ domain.TenantID, m *gitworker.Material) { m.Token = nil },
		"a token with whitespace":          func(_ domain.TenantID, m *gitworker.Material) { m.Token = []byte("a b") },
		"a token that is a PEM key":        func(_ domain.TenantID, m *gitworker.Material) { m.Token = keyMaterial.privatePEM },
		"no public keys":                   func(_ domain.TenantID, m *gitworker.Material) { m.PublicKeys = nil },
		"no CA":                            func(_ domain.TenantID, m *gitworker.Material) { m.CA = nil },
		"an EC private key as the keys":    func(_ domain.TenantID, m *gitworker.Material) { m.PublicKeys = keyMaterial.privatePEM },
		"a PKCS#8 private key as the keys": func(_ domain.TenantID, m *gitworker.Material) { m.PublicKeys = keyMaterial.pkcs8PEM },
		"a private key beside a public key": func(_ domain.TenantID, m *gitworker.Material) {
			m.PublicKeys = append(append([]byte(nil), keyMaterial.publicPEM...), keyMaterial.privatePEM...)
		},
		"a private key in the CA": func(_ domain.TenantID, m *gitworker.Material) {
			m.CA = append(append([]byte(nil), keyMaterial.certPEM...), keyMaterial.pkcs8PEM...)
		},
		"a public key as the CA":    func(_ domain.TenantID, m *gitworker.Material) { m.CA = keyMaterial.publicPEM },
		"a certificate as the keys": func(_ domain.TenantID, m *gitworker.Material) { m.PublicKeys = keyMaterial.certPEM },
		"a malformed public key": func(_ domain.TenantID, m *gitworker.Material) {
			m.PublicKeys = []byte("-----BEGIN PUBLIC KEY-----\nAAAA\n-----END PUBLIC KEY-----\n")
		},
		"text before the key": func(_ domain.TenantID, m *gitworker.Material) {
			m.PublicKeys = append([]byte("note\n"), keyMaterial.publicPEM...)
		},
		"text after the key": func(_ domain.TenantID, m *gitworker.Material) {
			m.PublicKeys = append(append([]byte(nil), keyMaterial.publicPEM...), "x"...)
		},
		"a PEM block with headers": func(_ domain.TenantID, m *gitworker.Material) {
			m.PublicKeys = bytesReplace(keyMaterial.publicPEM, "-----BEGIN PUBLIC KEY-----\n", "-----BEGIN PUBLIC KEY-----\nX: y\n\n")
		},
		"an unterminated private key before a public key": func(_ domain.TenantID, m *gitworker.Material) {
			m.PublicKeys = append(unterminated(keyMaterial.pkcs8PEM), keyMaterial.publicPEM...)
		},
		"an unterminated EC private key before a public key": func(_ domain.TenantID, m *gitworker.Material) {
			m.PublicKeys = append(unterminated(keyMaterial.privatePEM), keyMaterial.publicPEM...)
		},
		"an unterminated public block before a public key": func(_ domain.TenantID, m *gitworker.Material) {
			m.PublicKeys = append(unterminated(keyMaterial.publicPEM), keyMaterial.publicPEM...)
		},
		"an unterminated private key before a CA": func(_ domain.TenantID, m *gitworker.Material) {
			m.CA = append(unterminated(keyMaterial.pkcs8PEM), keyMaterial.certPEM...)
		},
		"garbage between two public keys": func(_ domain.TenantID, m *gitworker.Material) {
			m.PublicKeys = append(append(append([]byte(nil), keyMaterial.publicPEM...), "AAAA base64 garbage\n"...), keyMaterial.publicPEM...)
		},
		"garbage between two certificates": func(_ domain.TenantID, m *gitworker.Material) {
			m.CA = append(append(append([]byte(nil), keyMaterial.certPEM...), "-----BEGIN NOTHING-----\n"...), keyMaterial.certPEM...)
		},
		"a public JWK set":            func(_ domain.TenantID, m *gitworker.Material) { m.PublicKeys = jwkPublic },
		"a private EC JWK":            func(_ domain.TenantID, m *gitworker.Material) { m.PublicKeys = jwkPrivate },
		"a private RSA JWK":           func(_ domain.TenantID, m *gitworker.Material) { m.PublicKeys = jwkRSA },
		"a symmetric JWK":             func(_ domain.TenantID, m *gitworker.Material) { m.PublicKeys = jwkOct },
		"an oversized public key set": func(_ domain.TenantID, m *gitworker.Material) { m.PublicKeys = make([]byte, 65<<10) },
	}
	for name, edit := range refused {
		t.Run("refuses "+name, func(t *testing.T) {
			e := newEnv(t)
			e.id.fn = edit
			_, err := e.rec.Reconcile(context.Background(), acme, e.spec)
			if !isErr(err, gitworker.ErrIdentity) {
				t.Fatalf("got %v", err)
			}
			if m := e.f.Mutations(); len(m) != 0 {
				t.Fatalf("an unusable identity left objects behind: %v", m)
			}
			if _, ok := e.f.secret(nsOf(acme)); ok {
				t.Fatal("a Secret exists")
			}
		})
	}

	t.Run("only validated blocks are delivered, re-encoded", func(t *testing.T) {
		e := newEnv(t)
		// Valid blocks with unusual line endings and surrounding whitespace.
		messy := func(b []byte) []byte {
			return []byte("\n  \r\n" + strings.ReplaceAll(string(b), "\n", "\r\n") + "\n\n")
		}
		e.id.fn = func(_ domain.TenantID, m *gitworker.Material) {
			m.PublicKeys = append(messy(keyMaterial.publicPEM), messy(keyMaterial.publicPEM)...)
			m.CA = messy(keyMaterial.certPEM)
		}
		e.reconcile(acme)
		s, _ := e.f.secret(nsOf(acme))
		if string(s.Data["public-keys"]) != string(keyMaterial.publicPEM)+string(keyMaterial.publicPEM) || string(s.Data["ca.crt"]) != string(keyMaterial.certPEM) {
			t.Fatalf("the delivered material is not the validated blocks re-encoded:\n%q\n%q", s.Data["public-keys"], s.Data["ca.crt"])
		}
		for k, v := range s.Data {
			if strings.Contains(string(v), "PRIVATE") {
				t.Fatalf("%s carries private material", k)
			}
		}
	})

	t.Run("several public keys are accepted", func(t *testing.T) {
		e := newEnv(t)
		e.id.fn = func(_ domain.TenantID, m *gitworker.Material) {
			m.PublicKeys = append(append([]byte(nil), keyMaterial.publicPEM...), keyMaterial.publicPEM...)
			m.CA = append(append([]byte(nil), keyMaterial.certPEM...), keyMaterial.certPEM...)
		}
		e.reconcile(acme)
	})

	t.Run("an identity provider outage is not a verdict", func(t *testing.T) {
		e := newEnv(t)
		e.id.err = errors.New("provider down")
		_, err := e.rec.Reconcile(context.Background(), acme, e.spec)
		if !isErr(err, gitworker.ErrUnavailable) || strings.Contains(err.Error(), "provider down") {
			t.Fatalf("got %v", err)
		}
		if len(e.f.Mutations()) != 0 {
			t.Fatal("objects were created without an identity")
		}
	})

	t.Run("a rotated token reaches the Secret without replacing the worker", func(t *testing.T) {
		e := newEnv(t)
		e.reconcile(acme)
		dep, _ := e.f.deployment(nsOf(acme))
		e.f.ResetCalls()
		e.reconcile(acme)
		if len(e.f.Mutations()) != 0 {
			t.Fatalf("an unchanged identity caused writes: %v", e.f.Mutations())
		}
		e.id.token = "rotated"
		e.reconcile(acme)
		if got := e.f.Mutations(); len(got) != 1 || got[0] != "update secret" {
			t.Fatalf("mutations %v", got)
		}
		s, _ := e.f.secret(nsOf(acme))
		if string(s.Data["token"]) != "rotated.t_acme" {
			t.Fatalf("token %q", s.Data["token"])
		}
		after, _ := e.f.deployment(nsOf(acme))
		if after.Metadata.UID != dep.Metadata.UID || after.Generation != dep.Generation {
			t.Fatal("a token rotation restarted the worker")
		}
	})

	t.Run("a tenant's worker is configured with its own identity only", func(t *testing.T) {
		e := newEnv(t)
		e.reconcile(acme)
		e.reconcile("t_beta")
		sa, _ := e.f.secret(nsOf(acme))
		sb, _ := e.f.secret(nsOf("t_beta"))
		if string(sa.Data["token"]) == string(sb.Data["token"]) || !strings.HasSuffix(string(sb.Data["token"]), "t_beta") {
			t.Fatalf("tokens %q %q", sa.Data["token"], sb.Data["token"])
		}
	})
}

func bytesReplace(b []byte, old, new string) []byte {
	return bytes.Replace(b, []byte(old), []byte(new), 1)
}

// unterminated returns a PEM block without its END line: pem.Decode skips it while
// it searches for the next valid block.
func unterminated(b []byte) []byte {
	i := bytes.Index(b, []byte("-----END"))
	return append([]byte(nil), b[:i]...)
}
