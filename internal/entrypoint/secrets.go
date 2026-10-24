package entrypoint

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
)

// secretSet is the materialized secret material of one attempt. Values never
// leave this struct except into the secrets directory and the child's
// environment; they are not logged.
type secretSet struct {
	// dir is the process-owned child directory the files live in ("" until one
	// is created); root is the supplied volume root it was created under. The
	// command is told dir, never root.
	dir    string
	root   string
	files  []string
	env    map[string]string
	values [][]byte
}

// envNameFor maps a binding name to its environment variable: upper-cased,
// every character outside [A-Za-z0-9_] replaced by "_", "_" prefixed if the
// result would start with a digit. "erp-token" becomes ERP_TOKEN.
func envNameFor(name string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(name) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	s := b.String()
	if s != "" && s[0] >= '0' && s[0] <= '9' {
		s = "_" + s
	}
	return s
}

// materializeSecrets resolves exactly the secrets declared in the context and
// delivers them: "file" bindings as <child>/<name> (0600), "env" bindings in the
// returned set's env map. An undeclared secret is never requested. The response
// must echo the binding it was requested for.
//
// root is the supplied secrets volume. Under the restricted Pod recipe it is a
// kubelet-created, memory-backed directory that the non-root user can write
// through fsGroup but does not own, so it can never be chmod-ed. It is therefore
// left exactly as found: the files go into a fresh child directory created
// exclusively (0700, named by the process) and owned by this process, and
// set.dir names that child. cleanup removes it on every exit path.
func materializeSecrets(ctx context.Context, rt Runtime, bindings []runtimev1.SecretBinding, root string) (*secretSet, error) {
	set := &secretSet{root: root, env: map[string]string{}}
	if len(bindings) == 0 {
		return set, nil
	}
	dir, err := makePrivateChild(root)
	if err != nil {
		return set, err
	}
	set.dir = dir
	for _, b := range bindings {
		resp, err := rt.ResolveSecret(ctx, runtimev1.SecretResolveRequest{Ref: b.Ref, Version: b.Version})
		if err != nil {
			return set, err
		}
		if resp.Ref != b.Ref || resp.Version != b.Version || resp.Name != b.Name || resp.Delivery != b.Delivery {
			return set, fmt.Errorf("secret response does not match binding %q", b.Name)
		}
		value, err := base64.StdEncoding.DecodeString(resp.ValueBase64)
		if err != nil {
			return set, fmt.Errorf("secret %q: invalid base64", b.Name)
		}
		set.values = append(set.values, value)
		switch b.Delivery {
		case runtimev1.DeliveryFile:
			if filepath.Base(b.Name) != b.Name {
				return set, fmt.Errorf("secret name %q is not a plain file name", b.Name)
			}
			p := filepath.Join(dir, b.Name)
			f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return set, fmt.Errorf("secret %q: %w", b.Name, err)
			}
			set.files = append(set.files, p)
			_, werr := f.Write(value)
			if cerr := f.Close(); werr == nil {
				werr = cerr
			}
			if werr != nil {
				return set, fmt.Errorf("secret %q: %w", b.Name, werr)
			}
		case runtimev1.DeliveryEnv:
			name := envNameFor(b.Name)
			if _, dup := set.env[name]; dup {
				return set, fmt.Errorf("secret %q maps to a duplicate environment variable", b.Name)
			}
			set.env[name] = string(value)
		}
	}
	return set, nil
}

// makePrivateChild creates an exclusive, private directory under root. It never
// changes the mode or owner of root itself, which may belong to someone else
// (the kubelet's volume). A missing root is created with its parents; a root
// that cannot be written, or a child that does not come back as a private
// directory owned by this process, fails the delivery: permissions are never
// relaxed and nothing falls back to another location.
func makePrivateChild(root string) (string, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("secrets directory: %w", err)
	}
	child, err := os.MkdirTemp(root, "kodama-secrets-")
	if err != nil {
		return "", fmt.Errorf("secrets directory: %w", err)
	}
	fi, err := os.Lstat(child)
	if err == nil {
		st, ok := fi.Sys().(*syscall.Stat_t)
		switch {
		case !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0:
			err = errors.New("not a directory")
		case fi.Mode().Perm()&0o077 != 0:
			err = errors.New("not private")
		case !ok || int(st.Uid) != os.Geteuid():
			err = errors.New("not owned by this process")
		}
	}
	if err != nil {
		_ = os.Remove(child)
		return "", fmt.Errorf("secrets directory: child %w", err)
	}
	return child, nil
}

// cleanup removes injected files and the child directory, and forgets values.
// It is idempotent and runs on every exit path of the step.
func (s *secretSet) cleanup() {
	for _, f := range s.files {
		_ = os.Remove(f)
	}
	if s.dir != "" {
		// The child is exclusively this process's: whatever the command left in it
		// is removed with it.
		_ = os.RemoveAll(s.dir)
		s.dir = ""
	}
	for _, v := range s.values {
		clear(v)
	}
	s.env = nil
}
