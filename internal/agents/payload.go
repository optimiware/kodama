package agents

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// The wrapper payload is the versioned, bounded, nonsecret configuration an
// agent step's wrapper receives. Runtime v1 has no dedicated instruction, model
// or tool-budget field, so the payload travels in the step's command arguments:
//
//	args[0]   PayloadMarker
//	args[1:]  the canonical payload JSON, base64url (unpadded), cut into
//	          PayloadChunk-sized arguments (every chunk full except the last)
//
// base64url keeps expression markers and shell metacharacters out of the
// arguments, so the execution module's reference resolution never interprets
// profile text. The same encoder and decoder serve the control plane and the
// wrapper; the payload holds no credential, token or tenant secret, and no
// identity: authority comes only from the attempt's workload identity.
const (
	// PayloadMarker opens the arguments of an expanded agent step.
	PayloadMarker = "kodama.agent-config/v1"
	// PayloadChunk is the length of every full argument chunk.
	PayloadChunk = 3072
	// PayloadVersion is the only payload version this module produces.
	PayloadVersion = 1
	// MaxPayloadBytes bounds the canonical payload JSON.
	MaxPayloadBytes = MaxContentBytes + 4<<10
	maxEncodedBytes = (MaxPayloadBytes*4 + 2) / 3
)

// ProfilePin identifies the exact profile version a run pinned.
type ProfilePin struct {
	Name    string `json:"name"`
	Version int64  `json:"version"`
	Digest  Digest `json:"digest"`
}

// WrapperConfig is the decoded payload: the pin and the complete pinned
// content, so the wrapper never re-reads a profile and the control plane can
// recompute the pin's digest from what it carries.
type WrapperConfig struct {
	Version int        `json:"version"`
	Profile ProfilePin `json:"profile"`
	Content Draft      `json:"content"`
}

// NewWrapperConfig builds the payload of one registered version.
func NewWrapperConfig(p ProfileVersion) (WrapperConfig, error) {
	if err := p.Validate(); err != nil {
		return WrapperConfig{}, err
	}
	return WrapperConfig{
		Version: PayloadVersion,
		Profile: ProfilePin{Name: p.Name, Version: int64(p.Version), Digest: p.Digest},
		Content: p.Content.Draft(),
	}, nil
}

// canonicalJSON returns the single canonical encoding of the payload.
func (c WrapperConfig) canonicalJSON() ([]byte, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("agents: encode payload: %w", err)
	}
	canonical, err := runtimev1.Canonicalize(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: the payload cannot be canonicalized", ErrInvalid)
	}
	if len(canonical) > MaxPayloadBytes {
		return nil, fmt.Errorf("%w: the payload is larger than %d bytes", ErrInvalid, MaxPayloadBytes)
	}
	return canonical, nil
}

// Args encodes the payload as command arguments.
func (c WrapperConfig) Args() ([]string, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	canonical, err := c.canonicalJSON()
	if err != nil {
		return nil, err
	}
	enc := base64.RawURLEncoding.EncodeToString(canonical)
	args := []string{PayloadMarker}
	for len(enc) > PayloadChunk {
		args = append(args, enc[:PayloadChunk])
		enc = enc[PayloadChunk:]
	}
	return append(args, enc), nil
}

// HasPayload reports whether the arguments open with the payload marker. It does
// not validate anything.
func HasPayload(args []string) bool { return len(args) > 0 && args[0] == PayloadMarker }

// DecodeArgs decodes and validates a payload. present is false (with a nil
// error) when the arguments are not an agent payload at all, which is how an
// ordinary container command is recognized. Everything else is strict:
// non-canonical chunking, invalid base64, unknown fields, trailing data, a
// payload that is not the single canonical encoding of its own content, a
// digest that does not match the content, and oversized input are errors.
func DecodeArgs(args []string) (cfg WrapperConfig, present bool, err error) {
	if !HasPayload(args) {
		return WrapperConfig{}, false, nil
	}
	bad := func(format string, a ...any) (WrapperConfig, bool, error) {
		return WrapperConfig{}, true, fmt.Errorf("%w: the agent payload %s", ErrInvalid, fmt.Sprintf(format, a...))
	}
	chunks := args[1:]
	if len(chunks) == 0 {
		return bad("is empty")
	}
	total := 0
	for i, ch := range chunks {
		total += len(ch)
		switch {
		case total > maxEncodedBytes:
			return bad("is too large")
		case ch == "" || len(ch) > PayloadChunk:
			return bad("has a malformed chunk")
		case i < len(chunks)-1 && len(ch) != PayloadChunk:
			return bad("is not canonically chunked")
		}
	}
	raw, derr := base64.RawURLEncoding.Strict().DecodeString(strings.Join(chunks, ""))
	if derr != nil {
		return bad("is not valid base64url")
	}
	if len(raw) > MaxPayloadBytes {
		return bad("is too large")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return bad("is not one well-formed object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return bad("has trailing data")
	}
	if err := cfg.validate(); err != nil {
		return bad("is invalid")
	}
	canonical, cerr := cfg.canonicalJSON()
	if cerr != nil || !bytes.Equal(canonical, raw) {
		return bad("is not canonical")
	}
	return cfg, true, nil
}

// validate checks the version, the content and that the pin describes it.
func (c WrapperConfig) validate() error {
	if c.Version != PayloadVersion {
		return fmt.Errorf("%w: unsupported payload version", ErrInvalid)
	}
	content, err := NewContent(c.Content)
	if err != nil {
		return err
	}
	if c.Profile.Digest != content.Digest() || c.Profile.Name != content.Name() {
		return fmt.Errorf("%w: the payload pin does not describe its content", ErrInvalid)
	}
	if c.Profile.Version < 1 || c.Profile.Version > domain.MaxSafeInteger {
		return fmt.Errorf("%w: the payload pin has no valid version", ErrInvalid)
	}
	// The normalized form must be what was carried, or the payload is not the
	// canonical encoding of the pinned content.
	raw, err := json.Marshal(c.Content)
	if err != nil {
		return fmt.Errorf("%w: the payload content cannot be encoded", ErrInvalid)
	}
	carried, err := runtimev1.Canonicalize(raw)
	if err != nil || !bytes.Equal(carried, content.canonical) {
		return fmt.Errorf("%w: the payload is not normalized", ErrInvalid)
	}
	return nil
}

// Verify checks that the payload agrees with the container command it travels
// with: the image and wrapper entrypoint of the pinned content.
func (c WrapperConfig) Verify(image string, command []string) error {
	if image != c.Content.Image || !slices.Equal(command, c.Content.Wrapper.Command) {
		return fmt.Errorf("%w: the payload does not match its command", ErrInvalid)
	}
	return nil
}

// Pins returns the runtime context pins the payload implies: the profile and
// the design baseline of the attempt.
func (c WrapperConfig) Pins() (runtimev1.Profile, runtimev1.DesignBaseline) {
	b := c.Content.DesignBaseline
	b.Requirements = slices.Clone(b.Requirements)
	b.Decisions = slices.Clone(b.Decisions)
	b.RequiredChecks = slices.Clone(b.RequiredChecks)
	b.Sources = slices.Clone(b.Sources)
	return runtimev1.Profile{Name: c.Profile.Name, Version: c.Profile.Version, WrapperVersion: c.Content.Wrapper.Version}, b
}

// Policy is the immutable, pinned policy the gateway and tool proxy consume. It
// narrows what the attempt may do; it never establishes workload authority.
type Policy struct {
	Profile        ProfilePin
	WrapperVersion string
	Model          Model
	Tools          []string
	Limits         Limits
	Transcript     Transcript
}

// Policy extracts the pinned policy.
func (c WrapperConfig) Policy() Policy {
	d := cloneDraft(c.Content)
	return Policy{Profile: c.Profile, WrapperVersion: d.Wrapper.Version, Model: d.Model, Tools: d.Tools, Limits: d.Limits, Transcript: d.Transcript}
}
