package runner

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
)

var testCommit = strings.Repeat("a", 40)

func sampleRequest() Request {
	return Request{Workspace: "kodama-t_acme-run_1", Credentials: GitCredentials{Dir: "/var/lib/kodama/git-credentials/t_acme_p_erp_run_1"},
		Sources: []CheckoutSource{
			{URL: "https://kodama.example.invalid/git/acme/erp/backend.git", Path: "backend", Commit: testCommit},
			{URL: "https://kodama.example.invalid/git/acme/context.git", Path: ContextTenantPath, Ref: "refs/heads/main", Context: true},
		}}
}

// Test_unit_kernel_run_podman_checkout_request: the Podman backend renders the
// checkout as one helper container with the run's volume and the credential
// directory (read-only, outside the volume), and refuses without an image.
func Test_unit_kernel_run_podman_checkout_request(t *testing.T) {
	t.Setenv(CheckoutImageEnv, "")
	if _, err := (Podman{}).checkoutArgs(sampleRequest()); !errors.Is(err, ErrCheckoutUnconfigured) {
		t.Fatalf("without an image: %v, want ErrCheckoutUnconfigured", err)
	}
	if err := (Podman{}).Checkout(t.Context(), sampleRequest()); !errors.Is(err, ErrCheckoutUnconfigured) || sourceReason(err) != ReasonSourceUnconfigured {
		t.Fatalf("Checkout without an image = %v (reason %q)", err, sourceReason(err))
	}

	t.Setenv(CheckoutImageEnv, "registry.example.invalid/kodama/git@sha256:"+strings.Repeat("b", 64))
	args, err := (Podman{}).checkoutArgs(sampleRequest())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"-v", "kodama-t_acme-run_1:/workspace",
		"/var/lib/kodama/git-credentials/t_acme_p_erp_run_1:/kodama/git:ro",
		"KODAMA_GIT_DIR=/kodama/git", "GIT_CONFIG_GLOBAL=/kodama/git/gitconfig", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0"} {
		if !slices.Contains(args, want) {
			t.Fatalf("podman %v lacks %q", args, want)
		}
	}
	image := slices.Index(args, "registry.example.invalid/kodama/git@sha256:"+strings.Repeat("b", 64))
	if image < 0 || args[image+1] != "/bin/sh" || args[image+2] != "-c" || image+4 != len(args) {
		t.Fatalf("image and command misplaced in %v", args)
	}
	script := args[len(args)-1]
	for _, want := range []string{
		"pin 'backend' 'https://kodama.example.invalid/git/acme/erp/backend.git' '" + testCommit + "'",
		"tip '.context/tenant' 'https://kodama.example.invalid/git/acme/context.git' 'refs/heads/main'",
		"checkout -q --detach", "remote add origin", "fetch -q --no-tags origin", "init -q --template=",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script lacks %q:\n%s", want, script)
		}
	}
	// No variable is inherited, and nothing carries a credential.
	for _, a := range args {
		if strings.HasPrefix(a, "HOME=") && a != "HOME=/tmp" {
			t.Fatalf("host HOME passed: %s", a)
		}
		if strings.Contains(a, "gitc_") || strings.Contains(a, "@kodama") {
			t.Fatalf("argument carries a credential: %s", a)
		}
	}
	if slices.Contains(args, "-v") && strings.Contains(strings.Join(args, " "), "/workspace:/workspace") {
		t.Fatal("the workspace is a host path, not the run's volume")
	}

	// A host path is not a volume; a relative credential directory is not mountable.
	bad := sampleRequest()
	bad.Workspace = "/tmp/ws"
	if _, err := (Podman{}).checkoutArgs(bad); err == nil {
		t.Fatal("a host path was accepted as the run's volume")
	}
	bad = sampleRequest()
	bad.Credentials.Dir = "git-credentials"
	if _, err := (Podman{}).checkoutArgs(bad); err == nil {
		t.Fatal("a relative credential directory was accepted")
	}
}

// Test_unit_kernel_run_checkout_script_refuses_unsafe_sources: the script is
// rendered only from validated, quoted values.
func Test_unit_kernel_run_checkout_script_refuses_unsafe_sources(t *testing.T) {
	ok := CheckoutSource{URL: "https://kodama.example.invalid/git/acme/erp/backend.git", Path: "backend", Commit: testCommit}
	if _, err := checkoutScript([]CheckoutSource{ok}); err != nil {
		t.Fatal(err)
	}
	root := ok
	root.Path = gitv1.RootPath
	if _, err := checkoutScript([]CheckoutSource{root}); err != nil {
		t.Fatalf("the root path: %v", err)
	}
	for name, mutate := range map[string]func(*CheckoutSource){
		"a credential in the URL": func(s *CheckoutSource) { s.URL = "https://kodama:gitc_x@kodama.example.invalid/git/a/b.git" },
		"a file URL":              func(s *CheckoutSource) { s.URL = "file:///etc" },
		"an ext URL":              func(s *CheckoutSource) { s.URL = "ext::sh -c id" },
		"a short commit":          func(s *CheckoutSource) { s.Commit = "abc1234" },
		"a ref as the commit":     func(s *CheckoutSource) { s.Commit = "refs/heads/main" },
		"an option as the commit": func(s *CheckoutSource) { s.Commit = "--upload-pack=x" },
		"a parent path":           func(s *CheckoutSource) { s.Path = "../x" },
		"an injected path":        func(s *CheckoutSource) { s.Path = "x'; id; '" },
		"a hidden path":           func(s *CheckoutSource) { s.Path = ".git" },
		"an absolute path":        func(s *CheckoutSource) { s.Path = "/etc" },
	} {
		s := ok
		mutate(&s)
		if _, err := checkoutScript([]CheckoutSource{s}); err == nil {
			t.Errorf("%s was rendered", name)
		}
	}
	ctx := CheckoutSource{URL: ok.URL, Path: ContextProjectPath, Ref: "refs/heads/main", Context: true}
	if _, err := checkoutScript([]CheckoutSource{ctx}); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*CheckoutSource){
		"a context over a source path": func(s *CheckoutSource) { s.Path = "backend" },
		"a context outside .context":   func(s *CheckoutSource) { s.Path = ".context/../x" },
		"a context tag":                func(s *CheckoutSource) { s.Ref = "refs/tags/v1" },
		"a context ref with a space":   func(s *CheckoutSource) { s.Ref = "refs/heads/a b" },
	} {
		s := ctx
		mutate(&s)
		if _, err := checkoutScript([]CheckoutSource{s}); err == nil {
			t.Errorf("%s was rendered", name)
		}
	}
	if got := shellQuote("a'b"); got != `'a'\''b'` {
		t.Fatalf("shellQuote = %s", got)
	}
}

// Test_unit_kernel_run_source_failure_reasons: a refused checkout configuration
// and an unavailable source have distinct stable reasons.
func Test_unit_kernel_run_source_failure_reasons(t *testing.T) {
	for err, want := range map[error]string{
		fmt.Errorf("%w: missing", ErrSourceUnavailable):         ReasonSourceUnavailable,
		fmt.Errorf("%w: no image", ErrCheckoutUnconfigured):     ReasonSourceUnconfigured,
		errors.New("anything else while preparing the sources"): ReasonSourceUnavailable,
	} {
		if got := sourceReason(err); got != want {
			t.Errorf("sourceReason(%v) = %q, want %q", err, got, want)
		}
	}
}
