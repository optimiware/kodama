// Package native wires the independent Git service to the native operating
// system: the private Unix-domain-socket hook callback channel, the
// `kodama git hook` client, bare-repository hook installation and a
// symlink-safe tenant storage view.
//
// It is composition glue only. It starts no process of its own, holds no
// business rules and never carries a raw actor credential: the hook channel
// carries only the opaque request-bound handle minted by the smart-HTTP front
// handler.
package native

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	// EnvHookSocket names the private socket path the backend environment
	// carries. It is not a secret and is the only extra variable the hook
	// process needs.
	EnvHookSocket = "KODAMA_HOOK_SOCKET"
)

// Phase is one native Git hook phase. The installer only installs the two
// batch phases; `update` exists for explicit invocation and is documented as
// unsafe to install because the single-ref adapter mutates the ref
// before receive-pack applies it.
type Phase string

const (
	// PhaseUpdate is the single-ref `update` hook phase.
	PhaseUpdate Phase = "update"
	// PhasePreReceive is the whole-batch pre-receive hook phase.
	PhasePreReceive Phase = "pre-receive"
	// PhasePostReceive is the whole-batch post-receive hook phase.
	PhasePostReceive Phase = "post-receive"
)

// Valid reports whether the phase is supported.
func (phase Phase) Valid() bool {
	switch phase {
	case PhaseUpdate, PhasePreReceive, PhasePostReceive:
		return true
	default:
		return false
	}
}

const (
	// DefaultTimeout bounds one hook callback when the configuration leaves it
	// unset.
	DefaultTimeout = 10 * time.Second
	// DefaultMaxRequestBytes bounds one encoded hook request.
	DefaultMaxRequestBytes = 64 << 10
	// DefaultMaxLines bounds one pre/post-receive batch.
	DefaultMaxLines = 256
	// DefaultMaxLineBytes bounds one stdin line.
	DefaultMaxLineBytes = 4096
	// DefaultMaxMessageBytes bounds the returned client diagnostic.
	DefaultMaxMessageBytes = 4096
	// MaxSocketPathBytes is the conservative sun_path bound shared by Linux
	// and the BSDs.
	MaxSocketPathBytes = 100
)

// ErrMalformedRequest reports an invalid hook callback request.
var ErrMalformedRequest = errors.New("malformed hook callback request")

// The only git object-environment variables the hook channel forwards. They
// are receive-pack quarantine paths, not credentials.
const (
	// EnvObjectDirectory names the quarantined object directory.
	EnvObjectDirectory = "GIT_OBJECT_DIRECTORY"
	// EnvAlternateObjectDirectories names the real repository object directory.
	EnvAlternateObjectDirectories = "GIT_ALTERNATE_OBJECT_DIRECTORIES"
	// EnvQuarantinePath names the receive-pack quarantine directory.
	EnvQuarantinePath = "GIT_QUARANTINE_PATH"
)

var hookObjectEnvironmentKeys = map[string]bool{
	EnvObjectDirectory:            true,
	EnvAlternateObjectDirectories: true,
	EnvQuarantinePath:             true,
}

// Request is one hook callback request. It carries only the opaque handle and
// the non-secret environment fields the smart-HTTP handler placed in the
// backend environment. It never carries a credential.
type Request struct {
	Phase       Phase    `json:"phase"`
	ActorRef    string   `json:"actor_ref"`
	Repository  string   `json:"repository"`
	ResourceID  string   `json:"resource_id"`
	SourceEvent string   `json:"source_event"`
	Ref         string   `json:"ref,omitempty"`
	Old         string   `json:"old,omitempty"`
	New         string   `json:"new,omitempty"`
	Lines       []string `json:"lines,omitempty"`
	// ObjectEnvironment carries the git receive-pack quarantine object
	// directories so the in-process ancestry check can see the pushed objects.
	// It is not a credential.
	ObjectEnvironment []string `json:"object_environment,omitempty"`
}

// Response is one hook callback decision. ExitCode is 0 to accept and 1 to
// reject. Message is a sanitized one-line diagnostic the hook binary prints to
// git.
type Response struct {
	ExitCode int    `json:"exit_code"`
	Message  string `json:"message,omitempty"`
}

// Accepted reports whether the callback accepted the hook invocation.
func (response Response) Accepted() bool {
	return response.ExitCode == 0
}

// Validate checks the phase-specific shape and every bounded field. It does
// not validate repository/resource/source identities: the git.Hook
// adapter performs that validation so the callback and in-process paths share
// exactly one implementation.
func (request Request) Validate() error {
	if !request.Phase.Valid() {
		return fmt.Errorf("%w: unknown phase %q", ErrMalformedRequest, request.Phase)
	}
	if request.ActorRef == "" || len(request.ActorRef) > 256 {
		return fmt.Errorf("%w: actor reference is empty or over-bound", ErrMalformedRequest)
	}
	if strings.TrimSpace(request.Repository) == "" || strings.TrimSpace(request.ResourceID) == "" ||
		strings.TrimSpace(request.SourceEvent) == "" {
		return fmt.Errorf("%w: missing hook environment", ErrMalformedRequest)
	}
	switch request.Phase {
	case PhaseUpdate:
		if request.Ref == "" || request.Old == "" || request.New == "" || len(request.Lines) != 0 {
			return fmt.Errorf("%w: update needs ref/old/new and no stdin lines", ErrMalformedRequest)
		}
	case PhasePreReceive, PhasePostReceive:
		if request.Ref != "" || request.Old != "" || request.New != "" {
			return fmt.Errorf("%w: %s takes stdin lines only", ErrMalformedRequest, request.Phase)
		}
		if len(request.Lines) == 0 || len(request.Lines) > DefaultMaxLines {
			return fmt.Errorf("%w: batch line count is out of bounds", ErrMalformedRequest)
		}
		for _, line := range request.Lines {
			if len(line) == 0 || len(line) > DefaultMaxLineBytes {
				return fmt.Errorf("%w: batch line is empty or over-bound", ErrMalformedRequest)
			}
		}
	}
	if len(request.ObjectEnvironment) > 3 {
		return fmt.Errorf("%w: object environment is over-bound", ErrMalformedRequest)
	}
	for _, entry := range request.ObjectEnvironment {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || !hookObjectEnvironmentKeys[key] || value == "" || len(value) > DefaultMaxLineBytes {
			return fmt.Errorf("%w: object environment entry is not allowed", ErrMalformedRequest)
		}
	}
	return nil
}

// LinesReader returns the exact stdin the git.Hook adapter parses.
func (request Request) LinesReader() io.Reader {
	return strings.NewReader(strings.Join(request.Lines, "\n") + "\n")
}

// encodeRequest marshals one request with a trailing newline frame.
func encodeRequest(request Request) ([]byte, error) {
	data, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("encode hook request: %w", err)
	}
	if len(data) > DefaultMaxRequestBytes {
		return nil, fmt.Errorf("%w: request exceeds %d bytes", ErrMalformedRequest, DefaultMaxRequestBytes)
	}
	return append(data, '\n'), nil
}

// decodeRequest strictly decodes exactly one request value and rejects unknown
// members, trailing data and an over-bound body.
func decodeRequest(data []byte) (Request, error) {
	if len(data) == 0 || len(data) > DefaultMaxRequestBytes {
		return Request{}, fmt.Errorf("%w: request size", ErrMalformedRequest)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var request Request
	if err := decoder.Decode(&request); err != nil {
		return Request{}, fmt.Errorf("%w: %v", ErrMalformedRequest, err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Request{}, fmt.Errorf("%w: trailing request data", ErrMalformedRequest)
	}
	if err := request.Validate(); err != nil {
		return Request{}, err
	}
	return request, nil
}

// decodeResponse strictly decodes exactly one response value.
func decodeResponse(data []byte) (Response, error) {
	if len(data) == 0 || len(data) > DefaultMaxRequestBytes {
		return Response{}, fmt.Errorf("%w: response size", ErrMalformedRequest)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var response Response
	if err := decoder.Decode(&response); err != nil {
		return Response{}, fmt.Errorf("%w: %v", ErrMalformedRequest, err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Response{}, fmt.Errorf("%w: trailing response data", ErrMalformedRequest)
	}
	if response.ExitCode != 0 && response.ExitCode != 1 {
		return Response{}, fmt.Errorf("%w: exit code %d", ErrMalformedRequest, response.ExitCode)
	}
	if len(response.Message) > DefaultMaxMessageBytes {
		return Response{}, fmt.Errorf("%w: message over-bound", ErrMalformedRequest)
	}
	return response, nil
}

// encodeResponse marshals one response with a trailing newline frame.
func encodeResponse(response Response) ([]byte, error) {
	if len(response.Message) > DefaultMaxMessageBytes {
		response.Message = ""
	}
	data, err := json.Marshal(response)
	if err != nil {
		return nil, fmt.Errorf("encode hook response: %w", err)
	}
	return append(data, '\n'), nil
}
