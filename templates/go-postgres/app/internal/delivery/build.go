package delivery

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// BuildInput describes one finished image build to verify and record.
type BuildInput struct {
	// Ref is the pushed image reference printed by the builder: repository@sha256:digest.
	Ref          string
	SourceCommit string
	// ToolImage is the digest-pinned tools image that executed the build. It is
	// required: evidence that does not identify the actual build environment is
	// not evidence.
	ToolImage string
	GoVersion string
	// Platforms is the explicit build platform set (os/arch) recorded with the
	// evidence; the pushed digest is an index over exactly these platforms.
	Platforms []string
	// ContextCommit, when set, is the pinned source commit from the runtime
	// context; the built commit must equal it.
	ContextCommit string
	// ContextToolImage, when set, is the command image the runtime context
	// pins for this step; the recorded tool identity must equal it.
	ContextToolImage string
	OutputsDir       string
	ResultsFile      string
	Images           ImageChecker
}

// BuildEvidence is the retained build record.
type BuildEvidence struct {
	SourceCommit string   `json:"sourceCommit"`
	ToolImage    string   `json:"toolImage"`
	GoVersion    string   `json:"goVersion"`
	Platforms    []string `json:"platforms"`
	Image        struct {
		Repository string `json:"repository"`
		Digest     string `json:"digest"`
	} `json:"image"`
}

// SplitRef splits repository[:tag]@sha256:digest. The digest is the identity;
// a tag the builder also printed is dropped.
func SplitRef(ref string) (repo, digest string, err error) {
	repo, digest, ok := strings.Cut(ref, "@")
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") && strings.Contains(repo, "/") {
		repo = repo[:i]
	}
	if !ok || !ValidRepository(repo) || !ValidDigest(digest) {
		return "", "", fmt.Errorf("delivery: %q is not a repository@sha256 reference", ref)
	}
	return repo, digest, nil
}

var commitRE = func(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

var platformRE = regexp.MustCompile(`^linux/(amd64|arm64)$`)

// ParsePlatforms validates a comma-separated build platform set: the
// development architectures, each once, in a stable order.
func ParsePlatforms(list string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, p := range strings.Split(list, ",") {
		p = strings.TrimSpace(p)
		if !platformRE.MatchString(p) {
			return nil, fmt.Errorf("delivery: %q is not a supported build platform (linux/amd64, linux/arm64)", p)
		}
		if seen[p] {
			return nil, fmt.Errorf("delivery: build platform %q is listed twice", p)
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// ErrToolIdentity reports a missing, mutable or mismatched tool identity.
var ErrToolIdentity = errors.New("delivery: the build needs a digest-pinned tools image that matches the pinned command image")

// VerifyBuild checks that the registry holds exactly the built digest and that
// it was built from the pinned commit, then retains the evidence and writes the
// OCI output. Promotion consumes the recorded digest; nothing rebuilds.
func VerifyBuild(ctx context.Context, in BuildInput) (BuildEvidence, error) {
	repo, digest, err := SplitRef(in.Ref)
	if err != nil {
		return BuildEvidence{}, err
	}
	if !commitRE(in.SourceCommit) {
		return BuildEvidence{}, errors.New("delivery: the source commit is not a full Git commit id")
	}
	if in.ContextCommit != "" && in.ContextCommit != in.SourceCommit {
		return BuildEvidence{}, fmt.Errorf("delivery: built commit %s is not the pinned commit %s", in.SourceCommit, in.ContextCommit)
	}
	toolRepo, toolDigest, err := SplitRef(in.ToolImage)
	if err != nil {
		return BuildEvidence{}, fmt.Errorf("%w: %q is not repository@sha256", ErrToolIdentity, in.ToolImage)
	}
	if in.ContextToolImage != "" {
		ctxRepo, ctxDigest, err := SplitRef(in.ContextToolImage)
		if err != nil || ctxRepo != toolRepo || ctxDigest != toolDigest {
			return BuildEvidence{}, fmt.Errorf("%w: built with %s, pinned %s", ErrToolIdentity, in.ToolImage, in.ContextToolImage)
		}
	}
	if len(in.Platforms) == 0 {
		return BuildEvidence{}, errors.New("delivery: the build platform set must be recorded")
	}
	platforms, err := ParsePlatforms(strings.Join(in.Platforms, ","))
	if err != nil {
		return BuildEvidence{}, err
	}
	got, err := in.Images.Digest(ctx, repo, digest)
	if err != nil {
		return BuildEvidence{}, fmt.Errorf("delivery: verifying the pushed image: %w", err)
	}
	if got != digest {
		return BuildEvidence{}, fmt.Errorf("delivery: the registry holds %s, not the built %s", got, digest)
	}
	var ev BuildEvidence
	ev.SourceCommit, ev.ToolImage, ev.GoVersion, ev.Platforms = in.SourceCommit, in.ToolImage, in.GoVersion, platforms
	ev.Image.Repository, ev.Image.Digest = repo, digest
	if err := WriteArtifact(in.OutputsDir, "build-metadata.json", ev); err != nil {
		return BuildEvidence{}, err
	}
	err = WriteResults(in.ResultsFile, "completed", "", map[string]any{"image": map[string]any{"digest": digest, "registryRef": repo}})
	return ev, err
}
