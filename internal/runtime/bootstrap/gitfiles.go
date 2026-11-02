package bootstrap

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
)

// Git files of a v3 (source set) workload: the opaque Git credential of the
// attempt's, or the preparation's, grant and the CA bundle to trust, delivered as
// files directly under /kodama/git in the workload (docs/contracts/runtime-v3.md,
// "Git credential delivery"). The runtime adapter owns the delivery only: it
// writes the files before the entrypoint starts, readable only by the workload,
// and removes them when the attempt or the preparation ends (a grant belongs to
// one workload and dies with it). The grant's issue and scope are the control
// plane's (T-150); the entrypoint's scoped credential helper reads the file and
// offers it to the Git service's URLs only (T-151).
//
// The runtime-issued credentials (seed, integration) are not written here: the
// entrypoint's `kodama step-git` writes them, with the same permissions, to the
// files the context advertises, after the control plane answered.

// GitDirectory is the directory the workload sees; dir passed to WriteGit is the
// host-side directory the runtime mounts there.
const GitDirectory = "/kodama/git"

var (
	gitFileRE       = regexp.MustCompile(runtimev3.Patterns["GitFile"])
	gitCredentialRE = regexp.MustCompile(gitv1.Patterns["Credential"])
)

// GitMaterial is what a runtime delivers for one grant. Credential is the opaque
// credential (the Git contract's syntax, never a username:password line); CA is the
// PEM trust bundle, empty to use the system roots.
type GitMaterial struct {
	Credential []byte
	CA         []byte
}

// String and GoString keep the credential out of formatted output.
func (GitMaterial) String() string   { return "bootstrap.GitMaterial{redacted}" }
func (GitMaterial) GoString() string { return "bootstrap.GitMaterial{redacted}" }

// ErrGitMaterial: the material or the delivery it is written for is invalid. The
// error never carries a credential.
var ErrGitMaterial = errors.New("invalid Git delivery")

// gitFileName maps a path the context names (/kodama/git/<file>) to its file name.
func gitFileName(declared string) (string, error) {
	if !gitFileRE.MatchString(declared) {
		return "", fmt.Errorf("%w: %q is not a file directly under %s", ErrGitMaterial, declared, GitDirectory)
	}
	return strings.TrimPrefix(declared, GitDirectory+"/"), nil
}

// WriteGit writes the attempt's Git files for its context's delivery: the
// credential at git.credentialFile and the CA at git.caFile, each 0600 and replaced
// atomically so a reader never sees a partial credential, in dir (created 0700 when
// absent). A delivery without a credential file (the provisioning run without
// sources) gets no credential and only the directory the entrypoint writes the seed
// credential into. The material must match the delivery exactly: a credential for a
// delivery that names no file, or the reverse, is refused.
func WriteGit(dir string, d runtimev3.GitDelivery, m GitMaterial) error {
	return writeGit(dir, d.CredentialFile, d.CAFile, m)
}

// WriteGitPreparation is WriteGit for the preparation workload's read-only grant.
func WriteGitPreparation(dir string, g runtimev3.PreparationGit, m GitMaterial) error {
	cred := g.CredentialFile
	return writeGit(dir, &cred, g.CAFile, m)
}

func writeGit(dir string, credentialFile, caFile *string, m GitMaterial) error {
	if (credentialFile != nil) != (len(m.Credential) > 0) {
		return fmt.Errorf("%w: the credential and the credential file the context names go together", ErrGitMaterial)
	}
	if (caFile != nil) != (len(m.CA) > 0) {
		return fmt.Errorf("%w: the CA bundle and the CA file the context names go together", ErrGitMaterial)
	}
	files := map[string][]byte{}
	if credentialFile != nil {
		name, err := gitFileName(*credentialFile)
		if err != nil {
			return err
		}
		// One line of the credential alphabet, no surrounding whitespace.
		if !gitCredentialRE.Match(m.Credential) {
			return fmt.Errorf("%w: the credential is not an opaque Git credential", ErrGitMaterial)
		}
		files[name] = append(append([]byte(nil), m.Credential...), '\n')
	}
	if caFile != nil {
		name, err := gitFileName(*caFile)
		if err != nil {
			return err
		}
		if _, dup := files[name]; dup {
			return fmt.Errorf("%w: the credential and the CA name the same file", ErrGitMaterial)
		}
		if err := validatePEMCerts(m.CA); err != nil {
			return fmt.Errorf("%w: %v", ErrGitMaterial, err)
		}
		files[name] = append([]byte(nil), m.CA...)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating the Git directory: %w", err)
	}
	for name, data := range files {
		if err := writeAtomic(filepath.Join(dir, name), data); err != nil {
			return fmt.Errorf("storing %s: %w", name, err)
		}
	}
	return nil
}

// RemoveGit removes every Git file of a workload, including the credentials the
// entrypoint wrote at runtime and their state: a grant dies with its attempt. It is
// idempotent and never removes dir's parent.
func RemoveGit(dir string) error {
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return err
	}
	var errs []error
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
