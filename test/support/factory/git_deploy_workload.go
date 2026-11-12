package factory

// Authored deploy-repository workload commands for the delivery path.
//
// The release action writes one deploy-repository candidate commit that records
// the exact tested digest, pushes it to the granted work ref and proves the
// same execution cannot move the approval-guarded deploy ref without a human
// approval. The integrate action re-reads the settled candidate/base/image
// outputs, requires the exact candidate checkout and ancestry, then moves the
// protected deploy ref with an explicit lease.
//
// The lost-response variant authors one additional branch: the first push goes
// to the caller's lossy relay and must fail after the server accepted it; the
// action then observes the protected ref through the ordinary relay and only
// exits successfully when that observation returns the candidate. It performs
// no second push, so a lost response can never create a duplicate deploy commit.
//
// Every command list is data executed by the product with `#!/bin/sh` and
// `set -e`. It reads the mounted execution credential for each Git
// authentication, supplies it only through `--config-env`, and never writes a
// credential into a workspace file or a repository configuration.

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/id"
)

const (
	// deployWorkloadProtectedRef is the only protected ref this path moves.
	deployWorkloadProtectedRef = "refs/heads/deploy"
	// deployWorkloadDirectory is the retained checkout of the deploy
	// repository shared by the release and integrate actions.
	deployWorkloadDirectory = "/workspace/git-deploy-repository"
	// deployWorkloadImagePath is the candidate file carrying the exact tested
	// digest-pinned image reference.
	deployWorkloadImagePath = "deploy/image"
	// deployWorkloadInputImage is the admitted digest-pinned promotion input.
	deployWorkloadInputImage = "/kodama/inputs/image"
	// deployWorkloadCandidateKey and deployWorkloadBaseKey are the release
	// outputs the approval subject and integrate action consume.
	deployWorkloadCandidateKey = "candidate"
	deployWorkloadBaseKey      = "expected_base"
	deployWorkloadImageKey     = "image"
	// Default action names of the deploy recipe.
	deployWorkloadDefaultReleaseAction   = "release"
	deployWorkloadDefaultApproveAction   = "approve"
	deployWorkloadDefaultIntegrateAction = "integrate"
)

// DeployCandidateSpec configures one authored deploy-repository action.
type DeployCandidateSpec struct {
	// WorkRef is the granted work branch the release action pushes.
	WorkRef string
	// ExpectedBase is the seeded deploy commit the candidate must build on.
	ExpectedBase string
	// ImageReference is the exact digest-pinned reference the candidate
	// records as its deploy image. The release action requires the admitted
	// `image` input to equal it, so promotion consumes the tested digest rather
	// than a freshly built one.
	ImageReference string
	// ReleaseAction, ApproveAction and IntegrateAction name the actions whose
	// outputs the later steps read. Empty selects the recipe defaults.
	ReleaseAction   string
	ApproveAction   string
	IntegrateAction string
	// LostPushURL, when set, is the lossy smart-HTTP origin the integrate
	// action pushes to exactly once. ObserveURL is the ordinary origin it
	// reads back before exiting without a repetition.
	LostPushURL string
	ObserveURL  string
}

// deployWorkloadSpec is the complete authored-command input. It carries no
// credential: every command reads the mounted execution token itself.
type deployWorkloadSpec struct {
	URL            string
	ObserveURL     string
	LostPushURL    string
	CAPEM          []byte
	TokenPath      string
	CAPath         string
	Workspace      string
	Directory      string
	OutputsRoot    string
	WorkRef        string
	ProtectedRef   string
	ExpectedBase   string
	ImageReference string

	ReleaseAction   string
	ApproveAction   string
	IntegrateAction string
}

// WorkloadDeployReleaseCommands authors the release action: clone the deploy
// repository, require the reviewed base, write the tested digest, commit one
// candidate, push it to the granted work ref, prove the protected ref refuses
// the unapproved update, and submit the candidate/base/image outputs.
func (fixture *GitFixture) WorkloadDeployReleaseCommands(t testing.TB, spec DeployCandidateSpec) ([]string, error) {
	t.Helper()
	internal, err := fixture.deploySpec(spec)
	if err != nil {
		return nil, err
	}
	return deployReleaseCommands(internal)
}

// WorkloadDeployIntegrateCommands authors the integrate action: require the
// settled outputs, the exact candidate checkout and base ancestry, then move
// the protected deploy ref with an explicit lease. With LostPushURL set it
// pushes once to the lossy origin, requires that attempt to fail, observes the
// accepted ref through the ordinary origin and exits without pushing again.
func (fixture *GitFixture) WorkloadDeployIntegrateCommands(t testing.TB, spec DeployCandidateSpec) ([]string, error) {
	t.Helper()
	internal, err := fixture.deploySpec(spec)
	if err != nil {
		return nil, err
	}
	return deployIntegrateCommands(internal)
}

// deploySpec fills the fixed container paths and validates caller scenario
// fields. It performs no effect.
func (fixture *GitFixture) deploySpec(spec DeployCandidateSpec) (deployWorkloadSpec, error) {
	if fixture == nil || fixture.h == nil || fixture.h.TLS == nil {
		return deployWorkloadSpec{}, errors.New("deploy workload commands need the private-CA TLS fixture")
	}
	internal := deployWorkloadSpec{
		URL:             fixture.WorkloadRepositoryURL(GitDeployRepository),
		ObserveURL:      spec.ObserveURL,
		LostPushURL:     spec.LostPushURL,
		CAPEM:           fixture.h.TLS.CAPEM,
		TokenPath:       gitWorkloadTokenPath,
		CAPath:          gitWorkloadCAFile,
		Workspace:       gitWorkloadWorkspace,
		Directory:       deployWorkloadDirectory,
		OutputsRoot:     protectedWorkloadOutputsRoot,
		WorkRef:         spec.WorkRef,
		ProtectedRef:    deployWorkloadProtectedRef,
		ExpectedBase:    spec.ExpectedBase,
		ImageReference:  spec.ImageReference,
		ReleaseAction:   spec.ReleaseAction,
		ApproveAction:   spec.ApproveAction,
		IntegrateAction: spec.IntegrateAction,
	}
	if internal.ObserveURL == "" {
		internal.ObserveURL = internal.URL
	}
	if internal.ReleaseAction == "" {
		internal.ReleaseAction = deployWorkloadDefaultReleaseAction
	}
	if internal.ApproveAction == "" {
		internal.ApproveAction = deployWorkloadDefaultApproveAction
	}
	if internal.IntegrateAction == "" {
		internal.IntegrateAction = deployWorkloadDefaultIntegrateAction
	}
	return internal, validateDeployWorkloadSpec(internal)
}

// deployReleaseCommands renders the release action.
func deployReleaseCommands(spec deployWorkloadSpec) ([]string, error) {
	if err := validateDeployWorkloadSpec(spec); err != nil {
		return nil, err
	}
	if spec.LostPushURL != "" {
		return nil, errors.New("the release action never takes the lossy push origin")
	}
	directory := shellQuote(spec.Directory)
	image := shellQuote(spec.ImageReference)
	base := shellQuote(spec.ExpectedBase)
	commands := deployPrelude(spec)
	commands = append(commands, deployOutputSubmitter(spec)...)
	commands = append(commands,
		"test -s "+shellQuote(deployWorkloadInputImage),
		"test \"$(cat "+shellQuote(deployWorkloadInputImage)+")\" = "+image,
		"rm -rf -- "+directory,
		"git_kodama clone --quiet "+shellQuote(spec.URL)+" "+directory,
		"cd "+directory,
		`test "$(git rev-parse HEAD)" = `+base,
		"mkdir -p deploy",
		"printf '%s\\n' "+image+" > "+shellQuote(deployWorkloadImagePath),
		"git add -- "+shellQuote(deployWorkloadImagePath),
		"git -c user.name=kodama -c user.email=kodama@example.invalid commit --quiet -m 'I06 W26 deploy candidate'",
		"kodama_deploy_candidate=$(git rev-parse HEAD)",
		`test "$kodama_deploy_candidate" != `+base,
		`test "$(git rev-parse HEAD^)" = `+base,
		"git_kodama push --quiet origin "+shellQuote("HEAD:"+spec.WorkRef),
		// The same execution cannot move the approval-guarded deploy ref
		// without a passed human request. An accepted push here is the
		// authority defect this control exists for.
		"if git_kodama push --quiet origin \"$kodama_deploy_candidate:"+spec.ProtectedRef+"\" >/dev/null 2>&1; then\n"+
			"  printf 'unapproved-deploy-push-accepted\\n' >&2\n"+
			"  exit 1\n"+
			"fi",
		"printf 'unapproved-deploy-push-refused\\n'",
	)
	commands = append(commands, workloadScanCommands(spec.TokenPath, spec.Workspace)...)
	commands = append(commands,
		"kodama_submit_outputs \"candidate=$kodama_deploy_candidate\" "+
			shellQuote("expected_base="+spec.ExpectedBase)+" "+
			shellQuote("image="+spec.ImageReference),
		"printf 'deploy-candidate-submitted\\n'",
	)
	return commands, nil
}

// deployIntegrateCommands renders the integrate action.
func deployIntegrateCommands(spec deployWorkloadSpec) ([]string, error) {
	if err := validateDeployWorkloadSpec(spec); err != nil {
		return nil, err
	}
	directory := shellQuote(spec.Directory)
	image := shellQuote(spec.ImageReference)
	base := shellQuote(spec.ExpectedBase)
	candidateFile := filepath.Join(spec.OutputsRoot, spec.ReleaseAction, deployWorkloadCandidateKey)
	baseFile := filepath.Join(spec.OutputsRoot, spec.ReleaseAction, deployWorkloadBaseKey)
	imageFile := filepath.Join(spec.OutputsRoot, spec.ReleaseAction, deployWorkloadImageKey)
	approvedFile := filepath.Join(spec.OutputsRoot, spec.ApproveAction, "approved")
	commands := deployPrelude(spec)
	commands = append(commands, deployOutputSubmitter(spec)...)
	commands = append(commands,
		"test -s "+shellQuote(candidateFile),
		"test -s "+shellQuote(baseFile),
		"test -s "+shellQuote(imageFile),
		"test -s "+shellQuote(approvedFile),
		"kodama_deploy_candidate=$(cat "+shellQuote(candidateFile)+")",
		"kodama_deploy_base=$(cat "+shellQuote(baseFile)+")",
		"kodama_deploy_image=$(cat "+shellQuote(imageFile)+")",
		`test "$kodama_deploy_base" = `+base,
		`test "$kodama_deploy_image" = `+image,
		`test "$(cat `+shellQuote(approvedFile)+`)" = 'approved'`,
		"cd "+directory,
		`test "$(git rev-parse HEAD)" = "$kodama_deploy_candidate"`,
		`test "$(git rev-parse HEAD^)" = `+base,
		`test "$(cat `+shellQuote(deployWorkloadImagePath)+`)" = "$kodama_deploy_image"`,
	)
	lease := shellQuote("--force-with-lease=" + spec.ProtectedRef + ":" + spec.ExpectedBase)
	if spec.LostPushURL != "" {
		commands = append(commands,
			// The real mutation is sent once to the lossy origin. A success
			// here would mean the fixture failed to lose the response, so the
			// scenario must fail rather than silently claim the loss.
			"if git_kodama push --quiet "+lease+" "+shellQuote(spec.LostPushURL)+
				" \"$kodama_deploy_candidate:"+spec.ProtectedRef+"\"; then\n"+
				"  printf 'lost-response-push-unexpectedly-succeeded\\n' >&2\n"+
				"  exit 1\n"+
				"fi",
			"kodama_deploy_observed=$(git_kodama ls-remote --refs "+shellQuote(spec.ObserveURL)+" "+shellQuote(spec.ProtectedRef)+" | cut -f1)",
			`test "$kodama_deploy_observed" = "$kodama_deploy_candidate"`,
			"printf 'observed-accepted-deploy-before-repetition\\n'",
		)
	} else {
		commands = append(commands,
			"git_kodama push --quiet "+lease+" origin \"$kodama_deploy_candidate:"+spec.ProtectedRef+"\"",
		)
	}
	commands = append(commands, workloadScanCommands(spec.TokenPath, spec.Workspace)...)
	commands = append(commands,
		"kodama_submit_outputs \"deployed=$kodama_deploy_candidate\"",
		"printf 'protected-deploy-integrated\\n'",
	)
	return commands, nil
}

// deployPrelude materializes the public CA and defines the one Git
// authentication function. It reuses the protected workload prelude so both
// paths authenticate identically.
func deployPrelude(spec deployWorkloadSpec) []string {
	return protectedWorkloadPrelude(protectedWorkloadSpec{
		CAPEM:     spec.CAPEM,
		CAPath:    spec.CAPath,
		TokenPath: spec.TokenPath,
	})
}

// deployOutputSubmitter renders the shared HTTPS submission functions.
func deployOutputSubmitter(spec deployWorkloadSpec) []string {
	return protectedWorkloadOutputSubmitter(protectedWorkloadSpec{
		TokenPath: spec.TokenPath,
		CAPath:    spec.CAPath,
		Workspace: spec.Workspace,
	})
}

// validateDeployWorkloadSpec refuses an unsafe command input before any
// command is rendered.
func validateDeployWorkloadSpec(spec deployWorkloadSpec) error {
	if err := validateWorkloadGitURL(spec.URL); err != nil {
		return err
	}
	if err := validateWorkloadCAPEM(spec.CAPEM); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"token": spec.TokenPath, "CA": spec.CAPath, "workspace": spec.Workspace,
		"directory": spec.Directory, "outputs root": spec.OutputsRoot,
	} {
		if !filepath.IsAbs(value) {
			return fmt.Errorf("deploy workload %s path %q is not absolute", name, value)
		}
	}
	workspace := strings.TrimSuffix(spec.Workspace, "/")
	if !strings.HasPrefix(spec.Directory, workspace+"/") {
		return fmt.Errorf("deploy workload directory %q is not inside %q", spec.Directory, spec.Workspace)
	}
	if !strings.HasPrefix(spec.WorkRef, "refs/heads/work/") {
		return fmt.Errorf("deploy workload ref %q is not a granted work branch", spec.WorkRef)
	}
	if spec.ProtectedRef != deployWorkloadProtectedRef {
		return fmt.Errorf("deploy workload ref %q is not the protected deploy ref", spec.ProtectedRef)
	}
	if !validProtectedCommit(spec.ExpectedBase) {
		return fmt.Errorf("deploy workload base %q is not an immutable commit identity", spec.ExpectedBase)
	}
	if !strings.Contains(spec.ImageReference, "@sha256:") ||
		strings.ContainsAny(spec.ImageReference, " \t\r\n") {
		return fmt.Errorf("deploy workload image %q is not a digest-pinned reference", spec.ImageReference)
	}
	if spec.LostPushURL != "" {
		if err := validateWorkloadGitURL(spec.LostPushURL); err != nil {
			return fmt.Errorf("deploy lossy push origin: %w", err)
		}
		if err := validateWorkloadGitURL(spec.ObserveURL); err != nil {
			return fmt.Errorf("deploy observation origin: %w", err)
		}
	}
	for name, value := range map[string]string{
		"release": spec.ReleaseAction, "approve": spec.ApproveAction,
		"integrate": spec.IntegrateAction,
	} {
		if value == "" {
			return fmt.Errorf("deploy workload %s action name is empty", name)
		}
		if _, err := id.ParseActionName(value); err != nil {
			return fmt.Errorf("deploy workload %s action: %w", name, err)
		}
	}
	return nil
}
