package podman_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The Podman test double. The test binary doubles as a `podman` executable: with
// KODAMA_FAKE_PODMAN set to a state directory it answers the few commands the
// runtime issues, records every `create` (its labels, volumes, environment and
// arguments) and every invocation, and touches no engine.
func TestMain(m *testing.M) {
	if dir := os.Getenv("KODAMA_FAKE_PODMAN"); dir != "" {
		os.Exit(fakePodman(dir, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// createdContainer is what the double records of one `podman create`.
type createdContainer struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Status     string            `json:"status"`
	Labels     map[string]string `json:"labels"`
	Volumes    []string          `json:"volumes"`
	Env        []string          `json:"env"`
	Tmpfs      []string          `json:"tmpfs"`
	Entrypoint string            `json:"entrypoint"`
	Args       []string          `json:"args"`
}

func fakePodman(dir string, args []string) int {
	if len(args) == 0 {
		return 2
	}
	if f, err := os.OpenFile(filepath.Join(dir, "calls.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		fmt.Fprintln(f, args[0])
		f.Close()
	}
	containers := filepath.Join(dir, "containers")
	_ = os.MkdirAll(containers, 0o700)
	load := func(ref string) (createdContainer, string, bool) {
		entries, _ := os.ReadDir(containers)
		for _, e := range entries {
			raw, err := os.ReadFile(filepath.Join(containers, e.Name()))
			if err != nil {
				continue
			}
			var c createdContainer
			if json.Unmarshal(raw, &c) == nil && (c.ID == ref || c.Name == ref) {
				return c, filepath.Join(containers, e.Name()), true
			}
		}
		return createdContainer{}, "", false
	}
	save := func(c createdContainer) {
		raw, _ := json.Marshal(c)
		_ = os.WriteFile(filepath.Join(containers, c.ID+".json"), raw, 0o600)
	}
	missing := func() int {
		fmt.Fprintln(os.Stderr, "Error: no such container")
		return 125
	}
	switch args[0] {
	case "create":
		c := createdContainer{Labels: map[string]string{}, Status: "created"}
		rest := args[1:]
		for i := 0; i < len(rest); i++ {
			value := func() string { i++; return rest[i] }
			switch a := rest[i]; a {
			case "--name":
				c.Name = value()
			case "--label":
				k, v, _ := strings.Cut(value(), "=")
				c.Labels[k] = v
			case "--volume":
				c.Volumes = append(c.Volumes, value())
			case "--env":
				c.Env = append(c.Env, value())
			case "--tmpfs":
				c.Tmpfs = append(c.Tmpfs, value())
			case "--entrypoint":
				c.Entrypoint = value()
				c.Args = rest[i+2:] // the image follows the entrypoint
				i = len(rest)
			case "--read-only", "--pull=missing", "--cap-drop=ALL", "--security-opt=no-new-privileges":
			default:
				if strings.HasPrefix(a, "--") {
					value() // a flag with its value
				}
			}
		}
		sum := sha256.Sum256([]byte(c.Name))
		c.ID = hex.EncodeToString(sum[:])
		save(c)
		fmt.Println(c.ID)
		return 0
	case "container":
		c, _, ok := load(args[len(args)-1])
		if !ok {
			return missing()
		}
		out, _ := json.Marshal([]map[string]any{{
			"Id": c.ID, "Name": "/" + c.Name, "Config": map[string]any{"Labels": c.Labels},
			"State": map[string]any{"Status": c.Status, "Running": c.Status == "running"},
		}})
		fmt.Println(string(out))
		return 0
	case "start":
		c, _, ok := load(args[len(args)-1])
		if !ok {
			return missing()
		}
		c.Status = "running"
		save(c)
		return 0
	case "stop", "rm":
		if _, path, ok := load(args[len(args)-1]); ok {
			_ = os.Remove(path)
		}
		return 0
	}
	return 0
}

// fakeEnv is a Podman double and its state directory.
type fakeEnv struct {
	// Binary is the path of the `podman` executable.
	Binary string
	dir    string
}

func newFake(t *testing.T) fakeEnv {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "podman")
	script := fmt.Sprintf("#!/bin/sh\nKODAMA_FAKE_PODMAN=%q exec %q \"$@\"\n", dir, exe)
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return fakeEnv{Binary: bin, dir: dir}
}

// calls returns the subcommand of every invocation, in order.
func (f fakeEnv) calls(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.dir, "calls.log"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(raw))
}

// created returns the recorded containers.
func (f fakeEnv) created(t *testing.T) []createdContainer {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Join(f.dir, "containers"))
	var out []createdContainer
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(f.dir, "containers", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var c createdContainer
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

// testCA returns a self-signed certificate as a PEM bundle.
func testCA(t *testing.T) []byte {
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
