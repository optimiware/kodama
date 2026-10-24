package agents

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// Bounds. They keep a profile, its wrapper payload and the pinned attempt
// context within the execution module's stored-size limits.
const (
	MaxInstructionBytes = 64 << 10
	MaxContentBytes     = 160 << 10
	MaxCommandItems     = 16
	MaxCommandItemBytes = 512
	MaxTools            = 128
	MaxWallSeconds      = 86400
	maxEndpointBytes    = 512
	maxImageBytes       = 512
)

var (
	imageRE     = regexp.MustCompile(runtimev1.Patterns["ImageRef"])
	versionRE   = regexp.MustCompile(runtimev1.Patterns["ClientVersion"])
	endpointRE  = regexp.MustCompile(runtimev1.Patterns["RepositoryURL"])
	identRE     = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,62}$`)
	modelNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)
	toolRE      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,127}$`)
)

// Harness identifies the pinned agent harness the wrapper drives.
type Harness struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

// Wrapper identifies the pinned command wrapper inside the image. Command is
// the exact entrypoint argv; compatibility evidence must cover it.
type Wrapper struct {
	ID      string   `json:"id"`
	Version string   `json:"version"`
	Command []string `json:"command"`
	Harness Harness  `json:"harness"`
}

// Model is the approved model configuration. Endpoint is credential-free by
// syntax: no userinfo, query or fragment. Credentials are the gateway's.
type Model struct {
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
}

// Limits are the claimed limits. WallSeconds is required; every other limit is
// claimed only when present, and a claimed limit must be positive.
type Limits struct {
	WallSeconds  int64  `json:"wallSeconds"`
	MaxRequests  *int64 `json:"maxRequests,omitempty"`
	MaxTokens    *int64 `json:"maxTokens,omitempty"`
	MaxToolCalls *int64 `json:"maxToolCalls,omitempty"`
}

// Transcript is the independently configurable transcript policy. Metadata
// (profile, result, usage references) is retained either way.
type Transcript struct {
	Record bool `json:"record"`
}

// Draft is the proposed content of a profile version, before validation. The
// JSON form is the stable wire form shared by storage and the wrapper payload.
type Draft struct {
	Name           string                   `json:"name"`
	Image          string                   `json:"image"`
	Wrapper        Wrapper                  `json:"wrapper"`
	Instructions   string                   `json:"instructions"`
	DesignBaseline runtimev1.DesignBaseline `json:"designBaseline"`
	Model          Model                    `json:"model"`
	Tools          []string                 `json:"tools"`
	Limits         Limits                   `json:"limits"`
	Transcript     Transcript               `json:"transcript"`
}

// Content is validated, normalized profile content with its canonical form and
// digest. It is immutable: the only way to obtain one is NewContent or
// ContentFromCanonical, and every accessor returns copies.
type Content struct {
	draft     Draft
	canonical []byte
	digest    Digest
}

// IsZero reports whether the content is unset.
func (c Content) IsZero() bool { return len(c.canonical) == 0 }

// Name returns the profile name.
func (c Content) Name() string { return c.draft.Name }

// Digest returns the SHA-256 digest of the canonical content.
func (c Content) Digest() Digest { return c.digest }

// Canonical returns a copy of the canonical UTF-8 JSON the digest covers.
func (c Content) Canonical() []byte { return append([]byte(nil), c.canonical...) }

// Draft returns a deep copy of the normalized content.
func (c Content) Draft() Draft { return cloneDraft(c.draft) }

// Claims lists the controls this content asks the installation to enforce, in
// the stable order of Controls.
func (c Content) Claims() []Control {
	d := c.draft
	claimed := map[Control]bool{ControlModel: true}
	if d.Limits.MaxRequests != nil {
		claimed[ControlRequests] = true
	}
	if d.Limits.MaxTokens != nil {
		claimed[ControlTokens] = true
	}
	if d.Limits.MaxToolCalls != nil {
		claimed[ControlToolCalls] = true
	}
	if len(d.Tools) > 0 {
		claimed[ControlToolAllowlist] = true
	}
	if d.Transcript.Record {
		claimed[ControlTranscript] = true
	}
	var out []Control
	for _, k := range Controls() {
		if claimed[k] {
			out = append(out, k)
		}
	}
	return out
}

func cloneInt(p *int64) *int64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func cloneDraft(d Draft) Draft {
	d.Wrapper.Command = append([]string{}, d.Wrapper.Command...)
	d.Tools = append([]string{}, d.Tools...)
	d.Limits.MaxRequests = cloneInt(d.Limits.MaxRequests)
	d.Limits.MaxTokens = cloneInt(d.Limits.MaxTokens)
	d.Limits.MaxToolCalls = cloneInt(d.Limits.MaxToolCalls)
	b := d.DesignBaseline
	b.Requirements = append([]string{}, b.Requirements...)
	b.Decisions = append([]string{}, b.Decisions...)
	b.RequiredChecks = append([]string{}, b.RequiredChecks...)
	b.Sources = append([]runtimev1.SourceRef{}, b.Sources...)
	d.DesignBaseline = b
	return d
}

func plainText(s string, allowControl bool) bool {
	if !utf8.ValidString(s) || strings.ContainsRune(s, 0) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			if allowControl && (r == '\n' || r == '\r' || r == '\t') {
				continue
			}
			return false
		}
	}
	return true
}

// NewContent validates and normalizes a draft. Normalization is limited to
// what is canonical by definition: tools are sorted and the design baseline is
// put in its canonical order with its digest computed (a supplied digest must
// match). The result is deterministic, so identical intent has one digest.
func NewContent(in Draft) (Content, error) {
	d := cloneDraft(in)
	var dg diagnostics
	field := func(path, format string, args ...any) { dg.add(CodeInvalidField, path, format, args...) }

	if !nameRE.MatchString(d.Name) {
		field("name", "the name must be a lowercase letter followed by lowercase letters, digits, underscores or hyphens, at most 63 characters")
	}
	if len(d.Image) > maxImageBytes || !imageRE.MatchString(d.Image) {
		field("image", "the image must be pinned by digest")
	}
	w := d.Wrapper
	if !identRE.MatchString(w.ID) {
		field("wrapper.id", "the wrapper identifier is not valid")
	}
	if !versionRE.MatchString(w.Version) {
		field("wrapper.version", "the wrapper version is not valid")
	}
	if !identRE.MatchString(w.Harness.ID) {
		field("wrapper.harness.id", "the harness identifier is not valid")
	}
	if !versionRE.MatchString(w.Harness.Version) {
		field("wrapper.harness.version", "the harness version is not valid")
	}
	if n := len(w.Command); n < 1 || n > MaxCommandItems {
		dg.add(CodeLimitExceeded, "wrapper.command", "the wrapper command needs 1..%d items", MaxCommandItems)
	}
	for i, a := range w.Command {
		if a == "" || len(a) > MaxCommandItemBytes || !plainText(a, false) || strings.Contains(a, "${{") {
			field(fmt.Sprintf("wrapper.command[%d]", i), "a command item must be 1..%d bytes of plain text without expressions", MaxCommandItemBytes)
		}
	}
	if n := len(d.Instructions); n < 1 || n > MaxInstructionBytes {
		dg.add(CodeLimitExceeded, "instructions", "the instructions must be 1..%d bytes", MaxInstructionBytes)
	} else if !plainText(d.Instructions, true) {
		field("instructions", "the instructions must be UTF-8 text without NUL or control characters")
	}
	if !modelNameRE.MatchString(d.Model.Name) {
		field("model.name", "the model name is not valid")
	}
	if len(d.Model.Endpoint) > maxEndpointBytes || !endpointRE.MatchString(d.Model.Endpoint) {
		field("model.endpoint", "the model endpoint must be a credential-free https URL without query or fragment")
	}
	if d.Limits.WallSeconds < 1 || d.Limits.WallSeconds > MaxWallSeconds {
		dg.add(CodeLimitExceeded, "limits.wallSeconds", "wallSeconds must be between 1 and %d", MaxWallSeconds)
	}
	for _, l := range []struct {
		path string
		v    *int64
	}{{"limits.maxRequests", d.Limits.MaxRequests}, {"limits.maxTokens", d.Limits.MaxTokens}, {"limits.maxToolCalls", d.Limits.MaxToolCalls}} {
		if l.v != nil && (*l.v < 1 || *l.v > domain.MaxSafeInteger) {
			dg.add(CodeLimitExceeded, l.path, "a claimed limit must be positive")
		}
	}
	if len(d.Tools) > MaxTools {
		dg.add(CodeLimitExceeded, "tools", "a profile allows at most %d tools", MaxTools)
	} else {
		sort.Strings(d.Tools)
		for i, t := range d.Tools {
			switch {
			case !toolRE.MatchString(t):
				field(fmt.Sprintf("tools[%d]", i), "the tool identifier is not valid")
			case i > 0 && d.Tools[i-1] == t:
				field(fmt.Sprintf("tools[%d]", i), "the tool is listed more than once")
			}
		}
	}

	// The design baseline is reference-only and immutable: sources carry Git
	// object IDs, never branches or tags, so the runtime contract's own checks
	// reject any moving reference.
	b := d.DesignBaseline
	baseline, berr := runtimev1.NewDesignBaseline(b.Requirements, b.Decisions, b.Sources, b.RequiredChecks)
	switch {
	case berr != nil:
		field("designBaseline", "the design baseline must list immutable requirement, decision, source and check references")
	case b.Digest != "" && b.Digest != baseline.Digest:
		field("designBaseline.digest", "the digest does not match the baseline references")
	default:
		d.DesignBaseline = baseline
	}
	if err := dg.err(); err != nil {
		return Content{}, err
	}

	raw, err := json.Marshal(d)
	if err != nil {
		return Content{}, fmt.Errorf("agents: encode content: %w", err)
	}
	canonical, err := runtimev1.Canonicalize(raw)
	if err != nil {
		return Content{}, fmt.Errorf("%w: the content cannot be canonicalized", ErrInvalid)
	}
	if len(canonical) > MaxContentBytes {
		dg.add(CodeLimitExceeded, "$", "the profile is larger than %d bytes", MaxContentBytes)
		return Content{}, dg.err()
	}
	sum := sha256.Sum256(canonical)
	return Content{draft: d, canonical: canonical, digest: Digest("sha256:" + hex.EncodeToString(sum[:]))}, nil
}

// DecodeDraft strictly decodes a draft: unknown fields and trailing data are
// rejected. Validation is NewContent's job.
func DecodeDraft(raw []byte) (Draft, error) {
	var d Draft
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return Draft{}, fmt.Errorf("%w: the profile is not one well-formed object", ErrInvalid)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Draft{}, fmt.Errorf("%w: unexpected data after the profile", ErrInvalid)
	}
	return d, nil
}

// ContentFromCanonical restores stored content and verifies that it is
// canonical, valid and has the recorded digest. Adapters call it on every row
// they load, so a corrupted record is never returned as a profile.
func ContentFromCanonical(canonical []byte, want Digest) (Content, error) {
	d, err := DecodeDraft(canonical)
	if err != nil {
		return Content{}, fmt.Errorf("%w: stored content is unreadable", ErrConflict)
	}
	c, err := NewContent(d)
	if err != nil {
		return Content{}, fmt.Errorf("%w: stored content is invalid", ErrConflict)
	}
	if !bytes.Equal(c.canonical, canonical) || c.digest != want {
		return Content{}, fmt.Errorf("%w: stored content does not match its digest", ErrConflict)
	}
	return c, nil
}

// ProfileVersion is an immutable, registered profile version: validated content
// owned by a project. There is no transition after creation. Changed content is
// registered as the next version of its name, identical content resolves to the
// existing record, and nothing updates or deletes a version.
type ProfileVersion struct {
	Scope     domain.Scope
	ID        ProfileID
	Name      string
	Version   domain.Version
	Digest    Digest
	Content   Content
	CreatedAt time.Time
	CreatedBy Actor
}

// Ref returns the immutable reference of this version.
func (p ProfileVersion) Ref() string { return Ref(p.Name, p.Version) }

// Candidate is validated content proposed for registration, carrying the
// identity the use case generated. The repository decides whether it becomes a
// new version or resolves to an existing identical one.
type Candidate struct {
	Scope     domain.Scope
	ID        ProfileID
	Content   Content
	CreatedBy Actor
	CreatedAt time.Time
	RequestID string
}

// Validate checks that a candidate is complete.
func (c Candidate) Validate() error {
	if err := c.Scope.Validate(); err != nil {
		return err
	}
	if err := c.ID.Validate(); err != nil {
		return err
	}
	if c.Content.IsZero() {
		return fmt.Errorf("%w: candidate has no validated content", domain.ErrInvalidValue)
	}
	if c.CreatedAt.IsZero() {
		return fmt.Errorf("%w: candidate has no creation time", domain.ErrInvalidValue)
	}
	return c.CreatedBy.Validate()
}

// NewProfileVersion creates the registered form of a candidate at the given
// version. It is the only constructor that turns validated content into a
// ProfileVersion.
func NewProfileVersion(c Candidate, version domain.Version) (ProfileVersion, error) {
	if err := c.Validate(); err != nil {
		return ProfileVersion{}, err
	}
	p := ProfileVersion{
		Scope: c.Scope, ID: c.ID, Name: c.Content.Name(), Version: version, Digest: c.Content.Digest(),
		Content: c.Content, CreatedAt: c.CreatedAt.UTC().Truncate(time.Microsecond), CreatedBy: c.CreatedBy,
	}
	if err := p.Validate(); err != nil {
		return ProfileVersion{}, err
	}
	return p, nil
}

// Validate checks the version's internal consistency. Persistence adapters
// call it on every row they load.
func (p ProfileVersion) Validate() error {
	if err := p.Scope.Validate(); err != nil {
		return err
	}
	if err := p.ID.Validate(); err != nil {
		return err
	}
	if err := p.Version.Validate(); err != nil {
		return err
	}
	if err := p.Digest.Validate(); err != nil {
		return err
	}
	if p.Content.IsZero() || p.Content.Digest() != p.Digest || p.Content.Name() != p.Name {
		return fmt.Errorf("%w: profile content does not match its name or digest", ErrConflict)
	}
	return p.CreatedBy.Validate()
}
