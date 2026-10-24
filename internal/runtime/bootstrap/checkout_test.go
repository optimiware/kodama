package bootstrap

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/execution"
)

const (
	testRepo   = "https://git.example.test/org/repo.git"
	testCommit = "0123456789abcdef0123456789abcdef01234567"
)

type providerFunc func(CheckoutRequest) (CheckoutMaterial, error)

func (f providerFunc) Checkout(_ context.Context, r CheckoutRequest) (CheckoutMaterial, error) {
	return f(r)
}

func readSpec(t *testing.T) (execution.AttemptSpec, runtimev1.Context) {
	t.Helper()
	s := specWith(t, func(c *runtimev1.Context) {
		c.Workspace = runtimev1.Workspace{Access: runtimev1.AccessRead, Source: &runtimev1.Source{RepositoryURL: testRepo, Commit: testCommit}}
	})
	s.Access, s.WorkspaceID = execution.AccessRead, "ws_1"
	c, err := s.DecodeContext()
	if err != nil {
		t.Fatal(err)
	}
	return s, c
}

func testPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "git-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func echo(r CheckoutRequest, ca []byte) CheckoutMaterial {
	return CheckoutMaterial{Scope: r.Scope, WorkspaceID: r.WorkspaceID, RepositoryURL: r.RepositoryURL, Credential: []byte("ro:pw"), GitCA: ca}
}

func Test_issue_checkout(t *testing.T) {
	ctx := context.Background()
	ca := testPEM(t)
	s, c := readSpec(t)
	never := providerFunc(func(CheckoutRequest) (CheckoutMaterial, error) {
		t.Fatal("the provider was consulted")
		return CheckoutMaterial{}, nil
	})

	t.Run("a workspace-free attempt consults no provider and gets nothing", func(t *testing.T) {
		free := specWith(t, nil)
		fc, err := free.DecodeContext()
		if err != nil {
			t.Fatal(err)
		}
		for _, provider := range []CheckoutProvider{never, nil} {
			if m, err := IssueCheckout(ctx, provider, free, fc); m != nil || err != nil {
				t.Fatalf("%v %v", m, err)
			}
		}
	})

	t.Run("the request is the committed attempt's pinned source", func(t *testing.T) {
		var got CheckoutRequest
		m, err := IssueCheckout(ctx, providerFunc(func(r CheckoutRequest) (CheckoutMaterial, error) { got = r; return echo(r, ca), nil }), s, c)
		if err != nil || m == nil {
			t.Fatalf("%v %v", m, err)
		}
		want := CheckoutRequest{
			Scope: s.Scope, RunID: s.RunID, StepID: s.StepID, AttemptID: s.AttemptID, WorkspaceID: "ws_1",
			Access: execution.AccessRead, RepositoryURL: testRepo, Commit: testCommit,
		}
		if got != want {
			t.Fatalf("%+v", got)
		}
		if out := m.String() + m.GoString(); strings.Contains(out, "pw") {
			t.Fatal("the material formats its credential")
		}
	})

	t.Run("no provider is ErrNoCheckout", func(t *testing.T) {
		if _, err := IssueCheckout(ctx, nil, s, c); !errors.Is(err, ErrNoCheckout) || !errors.Is(err, execution.ErrNotConfigured) {
			t.Fatal(err)
		}
	})

	t.Run("an inconsistent attempt is invalid before the provider is asked", func(t *testing.T) {
		differs := s
		differs.Access = execution.AccessWrite
		unbound := s
		unbound.WorkspaceID = ""
		nosrc := c
		nosrc.Workspace.Source = nil
		for name, tc := range map[string]struct {
			s execution.AttemptSpec
			c runtimev1.Context
		}{"access differs": {differs, c}, "no workspace binding": {unbound, c}, "no source": {s, nosrc}} {
			if _, err := IssueCheckout(ctx, never, tc.s, tc.c); !errors.Is(err, execution.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
	})

	t.Run("the credential is copied", func(t *testing.T) {
		cred := []byte("ro:pw")
		m, err := IssueCheckout(ctx, providerFunc(func(r CheckoutRequest) (CheckoutMaterial, error) {
			x := echo(r, ca)
			x.Credential = cred
			return x, nil
		}), s, c)
		if err != nil {
			t.Fatal(err)
		}
		cred[0] = 'X'
		if string(m.Credential) != "ro:pw" {
			t.Fatal("the material aliases the provider's buffer")
		}
	})
}

func Test_write_checkout(t *testing.T) {
	dir, ca := t.TempDir(), testPEM(t)
	m := CheckoutMaterial{Credential: []byte("ro:pw"), GitCA: ca}
	if err := WriteCheckout(dir, m); err != nil {
		t.Fatal(err)
	}
	m.Credential = []byte("ro:pw2")
	if err := WriteCheckout(dir, m); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{CheckoutCredentialFile: "ro:pw2", GitCAFile: string(ca)} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(raw) != want {
			t.Fatalf("%s: %q %v", name, raw, err)
		}
		if fi, _ := os.Stat(filepath.Join(dir, name)); fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v", name, fi.Mode())
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Fatalf("temporary files remain: %v", entries)
	}
	if err := WriteCheckout(filepath.Join(dir, "missing"), m); err == nil {
		t.Fatal("wrote into a missing directory")
	}
}
