//go:build qualification

package production

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestHarnessKubectlRejectsExtraFlags proves every unlisted option and every
// scope-changing or raw-read flag is refused before a kubectl process starts.
func TestHarnessKubectlRejectsExtraFlags(t *testing.T) {
	cases := []struct {
		name  string
		extra []string
	}{
		{name: "namespace equals", extra: []string{"--namespace=kube-system", "--all"}},
		{name: "all", extra: []string{"--all"}},
		{name: "namespace bare", extra: []string{"--namespace"}},
		{name: "raw bare", extra: []string{"--raw"}},
		{name: "token bare", extra: []string{"--token"}},
		{name: "namespace short", extra: []string{"-n", "kube-system"}},
		{name: "namespace short equals", extra: []string{"-n=kube-system"}},
		{name: "context", extra: []string{"--context=kodama-probe"}},
		{name: "all namespaces", extra: []string{"--all-namespaces"}},
		{name: "all namespaces short", extra: []string{"-A"}},
		{name: "kubeconfig", extra: []string{"--kubeconfig=/tmp/other"}},
		{name: "server", extra: []string{"--server=https://evil"}},
		{name: "token", extra: []string{"--token=x"}},
		{name: "as", extra: []string{"--as=system:admin"}},
		{name: "raw node read", extra: []string{"--raw=/api/v1/nodes"}},
		{name: "cluster", extra: []string{"--cluster=other"}},
		{name: "user", extra: []string{"--user=admin"}},
		{name: "insecure tls", extra: []string{"--insecure-skip-tls-verify"}},
		{name: "positional slash escape", extra: []string{"pod/../other"}},
		{name: "positional whitespace", extra: []string{"pod name"}},
		{name: "positional query", extra: []string{"pod?x=1"}},
		{name: "output path escape", extra: []string{"--output=../../etc/passwd"}},
		{name: "watch streaming", extra: []string{"--watch=true"}},
		{name: "watch bare", extra: []string{"--watch"}},
		{name: "missing output value", extra: []string{"-o"}},
		{name: "forbidden after allowed", extra: []string{"-o", "json", "--namespace=kube-system"}},
		{name: "single dash selector", extra: []string{"-selector=//evil.example.com"}},
		{name: "single dash since", extra: []string{"-since=5s"}},
		{name: "single dash for", extra: []string{"-for=condition=Ready"}},
		{name: "single dash output", extra: []string{"-output=json"}},
		{name: "single dash field selector", extra: []string{"-field-selector=a=b"}},
		{name: "single dash timeout", extra: []string{"-timeout=30s"}},
		{name: "single dash tail", extra: []string{"-tail=10"}},
		{name: "single dash replicas", extra: []string{"-replicas=1"}},
		{name: "single dash watch", extra: []string{"-watch=false"}},
		{name: "single dash attached output", extra: []string{"-oyaml"}},
		{name: "single dash attached output equals", extra: []string{"-o=json"}},
		{name: "single dash attached selector equals", extra: []string{"-l=app=x"}},
		{name: "single dash attached selector", extra: []string{"-lapp=x"}},
		{name: "watch separate value", extra: []string{"--watch", "false"}},
		{name: "replicas too large", extra: []string{"--replicas=11"}},
		{name: "grace period too large", extra: []string{"--grace-period=301"}},
		{name: "timeout too many digits", extra: []string{"--timeout=1234567s"}},
		{name: "timeout no unit", extra: []string{"--timeout=30"}},
		{name: "tail too large", extra: []string{"--tail=10001"}},
		{name: "tail not a number", extra: []string{"--tail=abc"}},
		{name: "since too many digits", extra: []string{"--since=1234567s"}},
		{name: "replicas negative attached", extra: []string{"--replicas=-1"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			client, marker := recordingKubectl(t)
			if _, err := client.Run(context.Background(), ReleaseNamespace, "get", "pods", testCase.extra...); err == nil {
				t.Fatalf("extra %v was admitted", testCase.extra)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("kubectl executed for refused extra %v", testCase.extra)
			}
		})
	}
}

// TestHarnessKubectlAcceptsAllowlistedExtraFlags proves the harness-needed
// options and safe positional names still reach kubectl.
func TestHarnessKubectlAcceptsAllowlistedExtraFlags(t *testing.T) {
	cases := []struct {
		name  string
		verb  string
		extra []string
	}{
		{name: "output short", verb: "get", extra: []string{"-o", "json"}},
		{name: "output long", verb: "get", extra: []string{"--output=json"}},
		{name: "jsonpath", verb: "get", extra: []string{"-o", "jsonpath={.spec.replicas}"}},
		{name: "selector short", verb: "get", extra: []string{"-l", "app=kodama"}},
		{name: "selector long", verb: "get", extra: []string{"--selector=app=kodama"}},
		{name: "replicas", verb: "scale", extra: []string{"--replicas=0"}},
		{name: "grace period", verb: "delete", extra: []string{"--grace-period=30"}},
		{name: "timeout", verb: "delete", extra: []string{"--timeout=30s"}},
		{name: "for", verb: "wait", extra: []string{"--for=condition=Ready"}},
		{name: "tail", verb: "logs", extra: []string{"--tail=100"}},
		{name: "since", verb: "logs", extra: []string{"--since=5m"}},
		{name: "field selector", verb: "get", extra: []string{"--field-selector=status.phase=Running"}},
		{name: "watch false", verb: "get", extra: []string{"--watch=false"}},
		{name: "positional name", verb: "get", extra: []string{"pod-1"}},
		{name: "replicas bound", verb: "scale", extra: []string{"--replicas=10"}},
		{name: "grace period bound", verb: "delete", extra: []string{"--grace-period=300"}},
		{name: "timeout bound", verb: "delete", extra: []string{"--timeout=999999s"}},
		{name: "tail bound", verb: "logs", extra: []string{"--tail=10000"}},
		{name: "tail all", verb: "logs", extra: []string{"--tail=-1"}},
		{name: "since bound", verb: "logs", extra: []string{"--since=999999m"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			client, marker := recordingKubectl(t)
			if _, err := client.Run(context.Background(), ReleaseNamespace, testCase.verb, "pods", testCase.extra...); err != nil {
				t.Fatalf("allowed extra %v was refused: %v", testCase.extra, err)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("kubectl did not execute for allowed extra %v: %v", testCase.extra, err)
			}
		})
	}
}

// TestHarnessKubectlNormalizesVerbAndResource proves kubectl executes the
// trimmed, lower-case values that passed the guard, never the caller spelling.
func TestHarnessKubectlNormalizesVerbAndResource(t *testing.T) {
	client, marker, argv := recordingKubectlWithArgv(t)
	if _, err := client.Run(context.Background(), ReleaseNamespace, " GET ", " Pods "); err != nil {
		t.Fatalf("spaced verb/resource was refused: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("kubectl did not execute: %v", err)
	}
	recorded, err := os.ReadFile(argv)
	if err != nil {
		t.Fatalf("read kubectl argv: %v", err)
	}
	if !strings.Contains(string(recorded), "get pods") {
		t.Fatalf("argv %q does not contain the normalized get pods", recorded)
	}
	if strings.Contains(string(recorded), " GET ") || strings.Contains(string(recorded), " Pods ") {
		t.Fatalf("argv %q contains the caller's unnormalized spelling", recorded)
	}
}

func recordingKubectl(t *testing.T) (*Kubectl, string) {
	client, marker, _ := recordingKubectlWithArgv(t)
	return client, marker
}

func recordingKubectlWithArgv(t *testing.T) (*Kubectl, string, string) {
	t.Helper()
	argv := t.TempDir() + "/argv"
	marker := t.TempDir() + "/marker"
	t.Setenv("Q02_FAKE_ARGV", argv)
	t.Setenv("Q02_FAKE_MARKER", marker)
	installFakeExecutable(t, "kubectl", `#!/bin/sh
printf '%s\n' "$*" >> "$Q02_FAKE_ARGV"
echo executed >> "$Q02_FAKE_MARKER"
exit 0
`)
	guard, err := NewGuard("kodama-stage-k8s", "kodama-tenant-tenant-a")
	if err != nil {
		t.Fatalf("build guard: %v", err)
	}
	return &Kubectl{Guard: guard, Path: "kubectl", Timeout: 5 * time.Second}, marker, argv
}

// TestHarnessKubectlFailureIsSecretFree proves a failing kubectl's stdout and
// stderr (including a credential-shaped string) never reaches the returned
// error.
func TestHarnessKubectlFailureIsSecretFree(t *testing.T) {
	marker := t.TempDir() + "/environment"
	t.Setenv("Q02_FAKE_MARKER", marker)
	t.Setenv("Q02_ADMIN_TOKEN", "subprocess-token-value")
	installFakeExecutable(t, "kubectl", `#!/bin/sh
printf '%s' "${Q02_ADMIN_TOKEN:-unset}" > "$Q02_FAKE_MARKER"
echo 'token=super-secret-value'
echo 'Authorization: Bearer super-secret-value' >&2
exit 1
`)
	guard, err := NewGuard("kodama-stage-k8s", "kodama-tenant-tenant-a")
	if err != nil {
		t.Fatalf("build guard: %v", err)
	}
	client := &Kubectl{Guard: guard, Path: "kubectl", Timeout: 5 * time.Second}
	_, err = client.Run(context.Background(), ReleaseNamespace, "delete", "pods", "pod-1")
	if err == nil {
		t.Fatal("a failing kubectl returned success")
	}
	for _, leaked := range []string{"super-secret-value", "token=", "Authorization:"} {
		if strings.Contains(err.Error(), leaked) {
			t.Fatalf("kubectl error leaked %q: %v", leaked, err)
		}
	}
	if !strings.Contains(err.Error(), "kubectl delete pods") {
		t.Fatalf("kubectl error %q does not name the refused operation", err)
	}
	data, readErr := os.ReadFile(marker)
	if readErr != nil {
		t.Fatalf("fake kubectl did not record its environment: %v", readErr)
	}
	if string(data) != "unset" {
		t.Fatalf("kubectl child received a credential-shaped environment variable: %q", data)
	}
}

// TestHarnessKubectlTimeoutCancels proves the bounded timeout terminates a
// stuck kubectl process and returns promptly.
func TestHarnessKubectlTimeoutCancels(t *testing.T) {
	installFakeExecutable(t, "kubectl", `#!/bin/sh
exec sleep 30
`)
	guard, err := NewGuard("kodama-stage-k8s", "kodama-tenant-tenant-a")
	if err != nil {
		t.Fatalf("build guard: %v", err)
	}
	client := &Kubectl{Guard: guard, Path: "kubectl", Timeout: 200 * time.Millisecond}

	started := time.Now()
	_, err = client.Run(context.Background(), ReleaseNamespace, "get", "pods")
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("a timed-out kubectl returned success")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("timeout took %s; the kubectl process was not cancelled", elapsed)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "timed out") {
		t.Fatalf("timeout error %q does not say it timed out", err)
	}
}

// TestHarnessKubectlRefusesBeforeExec proves the guard is applied before any
// process starts.
func TestHarnessKubectlRefusesBeforeExec(t *testing.T) {
	marker := t.TempDir() + "/executed"
	installFakeExecutable(t, "kubectl", `#!/bin/sh
echo executed >> "$Q02_FAKE_MARKER"
exit 0
`)
	t.Setenv("Q02_FAKE_MARKER", marker)
	guard, err := NewGuard("kodama-stage-k8s", "kodama-tenant-tenant-a")
	if err != nil {
		t.Fatalf("build guard: %v", err)
	}
	client := &Kubectl{Guard: guard, Path: "kubectl", Timeout: 5 * time.Second}
	if _, err := client.Run(context.Background(), "default", "delete", "pods"); err == nil {
		t.Fatal("kubectl in another namespace was admitted")
	}
	if _, err := client.Run(context.Background(), ReleaseNamespace, "delete", "nodes"); err == nil {
		t.Fatal("a node mutation was admitted")
	}
	if _, err := client.Run(context.Background(), ReleaseNamespace, "get", "pods"); err != nil {
		t.Fatalf("allowed read failed: %v", err)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("fake kubectl was not executed for the allowed read: %v", err)
	}
	if lines := strings.Split(strings.TrimSpace(string(data)), "\n"); len(lines) != 1 || lines[0] != "executed" {
		t.Fatalf("fake kubectl executed %d time(s) for one allowed read; refused operations must not execute: %q", len(lines), data)
	}
}
