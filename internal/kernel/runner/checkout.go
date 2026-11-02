package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// CheckoutImageEnv names the image a Podman backend runs its checkout helper in.
// It must provide /bin/sh and git.
const CheckoutImageEnv = "KODAMA_GIT_CHECKOUT_IMAGE"

// maxCheckoutOutput bounds the Git output kept in an error.
const maxCheckoutOutput = 2048

var (
	_ Checkout = Process{}
	_ Checkout = Podman{}
)

// gitEnvironment is the whole environment of a checkout: nothing is inherited
// but PATH. System and user configuration are ignored, the one configuration is
// the run's private file outside the workspace, and Git never prompts.
func gitEnvironment(gitDir, home string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + filepath.Join(gitDir, GitConfigFile),
		"GIT_TERMINAL_PROMPT=0",
		"KODAMA_GIT_DIR=" + gitDir,
	}
}

// Checkout runs the host git binary with a sanitized environment, placing each
// source in <workspace>/<path>. The credential comes from the run's credential
// helper configuration, never from the URL or the repository's configuration.
func (p Process) Checkout(ctx context.Context, req Request) error {
	workspace, err := filepath.Abs(req.Workspace)
	if err != nil || req.Workspace == "" {
		return errors.New("checkout: the workspace must be a directory")
	}
	credDir, err := filepath.Abs(req.Credentials.Dir)
	if err != nil || req.Credentials.Dir == "" || within(workspace, credDir) {
		return errors.New("checkout: the credential directory must be outside the workspace")
	}
	script, err := checkoutScript(req.Sources)
	if err != nil {
		return err
	}
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("checkout: git is not installed: %w", err)
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
	cmd.Dir = workspace
	cmd.Env = gitEnvironment(credDir, credDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("checkout: %v: %s", err, tail(out))
	}
	return nil
}

// within reports whether path is dir or lies beneath it.
func within(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func tail(out []byte) string {
	s := strings.TrimSpace(string(out))
	if len(s) > maxCheckoutOutput {
		s = "…" + s[len(s)-maxCheckoutOutput:]
	}
	return s
}

func (p Podman) checkoutImage() string {
	if p.CheckoutImage != "" {
		return p.CheckoutImage
	}
	return os.Getenv(CheckoutImageEnv)
}

// checkoutArgs renders the podman invocation of the checkout helper: the run's
// volume at /workspace, the credential directory read-only at /kodama/git and the
// same script and sanitized Git environment as Process.
func (p Podman) checkoutArgs(req Request) ([]string, error) {
	image := p.checkoutImage()
	if image == "" {
		return nil, fmt.Errorf("%w: %s is not set, so the Podman backend cannot check sources out", ErrCheckoutUnconfigured, CheckoutImageEnv)
	}
	if req.Workspace == "" || strings.ContainsAny(req.Workspace, "/:") {
		return nil, errors.New("checkout: the workspace is not a volume name")
	}
	if !filepath.IsAbs(req.Credentials.Dir) {
		return nil, errors.New("checkout: the credential directory must be absolute")
	}
	script, err := checkoutScript(req.Sources)
	if err != nil {
		return nil, err
	}
	args := []string{"run", "--rm", "--name", "kodama-checkout-" + sanitize(req.Workspace),
		"-v", req.Workspace + ":/workspace", "-v", req.Credentials.Dir + ":" + GitContainerDir + ":ro", "-w", "/workspace"}
	if n := os.Getenv("KODAMA_PODMAN_NETWORK"); n != "" {
		args = append(args, "--network", n)
	}
	for _, e := range gitEnvironment(GitContainerDir, "/tmp") {
		if strings.HasPrefix(e, "PATH=") {
			continue // the image's own PATH
		}
		args = append(args, "-e", e)
	}
	return append(args, image, "/bin/sh", "-c", script), nil
}

// Checkout performs the same checkout inside a helper container with the run's
// volume mounted. Without KODAMA_GIT_CHECKOUT_IMAGE it refuses with
// ErrCheckoutUnconfigured instead of pretending the workspace is checked out.
func (p Podman) Checkout(ctx context.Context, req Request) error {
	args, err := p.checkoutArgs(req)
	if err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, p.bin(), args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("podman checkout: %v: %s", err, tail(out))
	}
	return nil
}
