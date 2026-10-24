package entrypoint

// T-079 acceptance: declared secrets materialize under the non-root Pod recipe.
//
// In the restricted Job the secrets volume root is created by the kubelet: the
// non-root user can write through it (fsGroup or a sticky world-writable mode)
// but does not own it, so chmod on it fails with EPERM. These tests use a real
// directory with exactly that property (a root-owned, sticky, world-writable
// directory such as /dev/shm) and the real operating-system chmod; nothing about
// the permission result is mocked. They need an unprivileged runner: as root
// every chmod succeeds and the failure cannot be reproduced.

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// foreignSecretsRoot returns a directory that this process can create entries
// in but does not own, and whose chmod by this process really fails. avoid lists
// paths it must neither contain nor lie in (the Config layout invariant).
func foreignSecretsRoot(t *testing.T, avoid ...string) string {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("the non-root secrets volume cannot be reproduced as root; run the entrypoint tests as an unprivileged user")
	}
candidates:
	for _, c := range []string{"/dev/shm", "/var/tmp", "/tmp"} {
		fi, err := os.Stat(c)
		if err != nil || !fi.IsDir() {
			continue
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) == os.Geteuid() {
			continue
		}
		for _, a := range avoid {
			if within(c, a) || within(a, c) {
				continue candidates
			}
		}
		probe, err := os.MkdirTemp(c, "kodama-t079-probe-")
		if err != nil {
			continue // not writable by this user
		}
		_ = os.Remove(probe)
		// The real chmod, passing the current mode so that a success (impossible
		// for a foreign-owned directory) could not change anything.
		if err := os.Chmod(c, fi.Mode()); err == nil {
			continue
		}
		return c
	}
	t.Fatal("no foreign-owned, writable directory to stand in for the kubelet's secrets volume")
	return ""
}

func Test_job_secret_dir(t *testing.T) {
	file := secretBinding("erp-token", "secret://project/erp-token", runtimev1.DeliveryFile)
	env := secretBinding("db-pass", "secret://project/db-pass", runtimev1.DeliveryEnv)
	const fileValue, envValue = "file-secret-value-4411", "env-secret-value-9922"

	t.Run("the old behavior really fails on a foreign-owned volume root", func(t *testing.T) {
		root := foreignSecretsRoot(t, t.TempDir())
		fi, err := os.Stat(root)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(root, 0o700); err == nil {
			t.Fatalf("chmod of %s succeeded: it is not a stand-in for the kubelet volume (mode was %v)", root, fi.Mode())
		}
	})

	t.Run("the real entrypoint delivers file and env secrets through an owned child", func(t *testing.T) {
		podRoot := t.TempDir()
		root := foreignSecretsRoot(t, podRoot)
		before, err := os.Stat(root)
		if err != nil {
			t.Fatal(err)
		}
		probe := `set -eu
d="$KODAMA_SECRETS_DIR"
mode() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"; }
owner() { stat -c %u "$1" 2>/dev/null || stat -f %u "$1"; }
{
  echo "dir=$d"
  echo "dirmode=$(mode "$d")"
  echo "diruid=$(owner "$d")"
  echo "uid=$(id -u)"
  echo "filemode=$(mode "$d/erp-token")"
  echo "filesha=$(sha256sum < "$d/erp-token" | cut -d' ' -f1)"
  echo "envsha=$(printf %s "$DB_PASS" | sha256sum | cut -d' ' -f1)"
} > "$KODAMA_OUTPUTS_DIR/probe.txt"
`
		p := newJCPod(t, jcOpts{
			script: probe, secretsRoot: root, secrets: []runtimev1.SecretBinding{file, env},
			values:  map[string]string{file.Ref: fileValue, env.Ref: envValue},
			outputs: []runtimev1.OutputDecl{{Name: "probe", Type: runtimev1.OutputArtifact, Path: ptr("/kodama/outputs/probe.txt")}},
		})
		run := p.run()
		if run.exit != ExitOK {
			t.Fatalf("exit = %d\nstdout:\n%s\nstderr:\n%s", run.exit, run.stdout, run.stderr)
		}
		if r := p.rt.result(t); r.Outcome != runtimev1.OutcomeCompleted {
			t.Fatalf("result = %+v\n%s", r, run.stderr)
		}
		raw, ok := p.rt.object("probe")
		if !ok {
			t.Fatal("the command did not report")
		}
		got := map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			k, v, _ := strings.Cut(line, "=")
			got[k] = v
		}
		// The command is told the actual child, not the volume root.
		child := got["dir"]
		if filepath.Dir(child) != root || child == root {
			t.Fatalf("KODAMA_SECRETS_DIR = %q, want a child of %q", child, root)
		}
		if got["dirmode"] != "700" {
			t.Errorf("child mode = %s, want 700", got["dirmode"])
		}
		if got["filemode"] != "600" {
			t.Errorf("secret file mode = %s, want 600", got["filemode"])
		}
		if got["diruid"] != got["uid"] || got["uid"] == "0" {
			t.Errorf("child owner uid %s, process uid %s: want the unprivileged process to own it", got["diruid"], got["uid"])
		}
		if got["filesha"] != sha256hex([]byte(fileValue)) {
			t.Error("the file secret was not delivered intact")
		}
		if got["envsha"] != sha256hex([]byte(envValue)) {
			t.Error("the env secret was not delivered intact")
		}
		// Cleaned up on exit; the volume root is exactly as found.
		if _, err := os.Stat(child); !os.IsNotExist(err) {
			t.Fatalf("the secrets child %s outlived the step (%v)", child, err)
		}
		after, err := os.Stat(root)
		if err != nil {
			t.Fatal(err)
		}
		if after.Mode() != before.Mode() {
			t.Fatalf("the volume root mode changed from %v to %v", before.Mode(), after.Mode())
		}
		// Neither value reached what the entrypoint wrote or sent.
		for _, v := range []string{fileValue, envValue} {
			for where, data := range map[string][]byte{"stdout": []byte(run.stdout), "stderr": []byte(run.stderr), "runtime API": p.rt.everything()} {
				if strings.Contains(string(data), v) {
					t.Errorf("a secret value reached %s", where)
				}
			}
		}
	})

	t.Run("a failed or revoked delivery still removes the child", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			err  error
			exit int
		}{
			{"declared secret refused", apiErr("resolveSecret", 403, domain.CodeForbidden), ExitOK},
			{"attempt revoked", apiErr("resolveSecret", 403, runtimev1.CodeAttemptRevoked), ExitRevoked},
		} {
			t.Run(tc.name, func(t *testing.T) {
				u := newUnit(t, `exit 0`)
				root := foreignSecretsRoot(t, u.root)
				u.cfg.SecretsDir = root
				// A name no other run can use, so the child is found by content.
				name := "erp-" + strings.ToLower(filepath.Base(u.root))
				name = strings.NewReplacer("_", "-", ".", "-").Replace(name)
				first := secretBinding(name, "secret://project/erp-first", runtimev1.DeliveryFile)
				second := secretBinding("db-second", "secret://project/db-second", runtimev1.DeliveryEnv)
				u.rt.ctx.Secrets = []runtimev1.SecretBinding{first, second}
				var child string
				u.rt.resolve = func(r runtimev1.SecretResolveRequest) (runtimev1.SecretResolveResponse, error) {
					if r.Ref == first.Ref {
						return runtimev1.SecretResolveResponse{Ref: first.Ref, Version: first.Version, Name: first.Name, Delivery: first.Delivery,
							ValueBase64: base64.StdEncoding.EncodeToString([]byte(fileValue))}, nil
					}
					matches, _ := filepath.Glob(filepath.Join(root, "kodama-secrets-*", name))
					if len(matches) == 1 {
						child = filepath.Dir(matches[0])
					}
					return runtimev1.SecretResolveResponse{}, tc.err
				}
				u.wantExit(u.run(), tc.exit)
				if child == "" {
					t.Fatal("the first secret was never delivered into a child of the volume root")
				}
				if _, err := os.Stat(child); !os.IsNotExist(err) {
					t.Fatalf("the secrets child %s outlived the failed delivery (%v)", child, err)
				}
			})
		}
	})

	t.Run("an unusable root fails the delivery without relaxing anything", func(t *testing.T) {
		// A regular file in place of the volume: no directory can be created
		// under it, and the entrypoint does not fall back anywhere else.
		blocker := filepath.Join(t.TempDir(), "secrets")
		if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		set, err := materializeSecrets(context.Background(), &fakeRuntime{secrets: map[string]runtimev1.SecretResolveResponse{file.Ref: response(file, fileValue)}},
			[]runtimev1.SecretBinding{file}, blocker)
		if err == nil {
			t.Fatal("delivery succeeded under a file")
		}
		if set.dir != "" {
			t.Fatalf("a directory %q was chosen despite the failure", set.dir)
		}
		set.cleanup()
	})
}
