package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/script"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Task metadata and conversation bounds. Metadata is one JSON object of string
// values; conversation bytes use the declared event data bound.
const (
	maxTaskMetadataBytes     = 16 << 10
	maxTaskConversationBytes = 64 << 10
)

// AdmittedAction is one resolved action row admitted with an execution. Index
// is the ordered position; Kind and Timeout are resolved at admission so the
// store persists them without re-deriving policy from YAML. Exactly one of
// Container or Request is present for the matching Kind. There is deliberately
// no admission-time deadline: it is set at activation.
type AdmittedAction struct {
	Index     int                   `json:"index"`
	Name      id.ActionName         `json:"name"`
	Kind      definition.ActionKind `json:"kind"`
	Timeout   time.Duration         `json:"timeout"`
	Container *definition.Container `json:"container,omitempty"`
	Request   *definition.Request   `json:"request,omitempty"`
}

// Validate checks the ordered index, name, kind, finite positive timeout and
// exactly one kind-specific declaration.
func (action AdmittedAction) Validate() error {
	if action.Index < 0 {
		return fault.New(fault.InvalidInput, "admitted action index is negative")
	}
	if _, err := id.ParseActionName(string(action.Name)); err != nil {
		return fault.Wrap(fault.InvalidInput, "admitted action name is invalid", err)
	}
	if !action.Kind.Valid() {
		return fault.New(fault.InvalidInput, fmt.Sprintf("admitted action kind %q is unknown", action.Kind))
	}
	if action.Timeout <= 0 {
		return fault.New(fault.InvalidInput, "admitted action timeout must be positive")
	}
	switch action.Kind {
	case definition.ActionKindContainer:
		if action.Container == nil || action.Request != nil {
			return fault.New(fault.InvalidInput, "container action requires exactly one container declaration")
		}
	case definition.ActionKindRequest:
		if action.Request == nil || action.Container != nil {
			return fault.New(fault.InvalidInput, "request action requires exactly one request declaration")
		}
	}
	return nil
}

// TaskContext is the optional task conversation/reference/metadata carried by
// one admission. TaskID is empty for a task-free inline run.
type TaskContext struct {
	TaskID       id.TaskID
	Conversation []byte
	References   []string
	Metadata     []byte
}

// AdmitRequest is one admission request. Definition carries either inline YAML
// or the identity of a stored pipeline to select. When both are present, the
// supplied exact YAML bytes are authoritative: the push trigger resumes a
// persisted first selection rather than re-reading an edited definition.
type AdmitRequest struct {
	Scope          tenancy.Scope
	Definition     Definition
	Inputs         map[id.InputKey]string
	Requirements   []string
	Task           TaskContext
	DefaultTimeout time.Duration
}

// AdmissionResult is the immutable admitted snapshot and its ordered actions.
type AdmissionResult struct {
	PipelineID      PipelineID
	PipelineVersion uint64
	Snapshot        Snapshot
	Actions         []AdmittedAction
}

// AdmissionConfig supplies the pre-admission registry resolver and the scoped
// definition reader. The reader is required only for stored definitions.
type AdmissionConfig struct {
	Resolver Resolver
	Reader   Reader
}

// AdmissionService orchestrates strict decode, input/reference validation,
// registry digest pinning and immutable snapshot construction. Registry calls
// stay outside every commit; this service never owns a commit port.
type AdmissionService struct {
	resolver Resolver
	reader   Reader
}

// NewAdmissionService validates the admission boundary dependencies.
func NewAdmissionService(config AdmissionConfig) (*AdmissionService, error) {
	if config.Resolver == nil {
		return nil, fault.New(fault.InternalFailure, "admission requires a registry resolver")
	}
	return &AdmissionService{resolver: config.Resolver, reader: config.Reader}, nil
}

// Admit selects one definition, validates its declarations and inputs, pins
// every image digest and returns an immutable snapshot. No action deadline is
// set; activation owns that.
func (service *AdmissionService) Admit(ctx context.Context, request AdmitRequest) (AdmissionResult, error) {
	if service == nil || service.resolver == nil {
		return AdmissionResult{}, fault.New(fault.InternalFailure, "admission service is not configured")
	}
	if err := request.Scope.Validate(); err != nil {
		return AdmissionResult{}, err
	}
	if !request.Scope.ProjectScoped() {
		return AdmissionResult{}, fault.New(fault.InvalidInput, "admission requires a project scope")
	}
	selected, err := service.selectDefinition(ctx, request)
	if err != nil {
		return AdmissionResult{}, err
	}
	parsed, err := decodeDocument(selected.YAML, DefaultLimits)
	if err != nil {
		return AdmissionResult{}, err
	}
	if err := ValidateInputs(parsed.inputs, request.Inputs); err != nil {
		return AdmissionResult{}, err
	}
	metadata, err := validateTaskContext(request.Task)
	if err != nil {
		return AdmissionResult{}, err
	}
	// The conversation belongs to a task. A task-free admission carries none,
	// even if a caller supplied stray bytes.
	var conversation []byte
	if request.Task.TaskID != "" {
		conversation = request.Task.Conversation
	}
	actions, images, secretNames, err := service.admitActions(ctx, parsed.pipeline, request.DefaultTimeout)
	if err != nil {
		return AdmissionResult{}, err
	}
	snapshot, err := NewSnapshot(SnapshotInput{
		YAML:         selected.YAML,
		Inputs:       request.Inputs,
		Env:          parsed.pipeline.Env,
		Images:       images,
		SecretNames:  secretNames,
		Requirements: request.Requirements,
		TaskID:       request.Task.TaskID,
		Conversation: conversation,
		References:   request.Task.References,
		Metadata:     metadata,
	})
	if err != nil {
		return AdmissionResult{}, err
	}
	return AdmissionResult{
		PipelineID:      selected.ID,
		PipelineVersion: selected.Version,
		Snapshot:        snapshot,
		Actions:         actions,
	}, nil
}

func (service *AdmissionService) selectDefinition(ctx context.Context, request AdmitRequest) (Definition, error) {
	// Exact supplied bytes are authoritative. The push trigger supplies the
	// persisted first-selection YAML together with its stored identity, and a
	// definition edit after that selection commit must never change the replay.
	// Inline starts use the same path with no stored identity.
	if len(request.Definition.YAML) > 0 {
		supplied := request.Definition
		supplied.YAML = append([]byte(nil), request.Definition.YAML...)
		if supplied.ID == "" {
			// An inline selection has no stored identity or version; a caller
			// supplied version is normalized away rather than echoed.
			supplied.Version = 0
			supplied.Push = nil
			supplied.PushTriggers = nil
		}
		return supplied, nil
	}
	if request.Definition.ID != "" {
		if service.reader == nil {
			return Definition{}, fault.New(fault.InvalidInput, "admission requires a definition reader for a stored pipeline")
		}
		selected, err := service.reader.Get(ctx, request.Scope, request.Definition.ID)
		if err != nil {
			return Definition{}, err
		}
		if len(selected.YAML) == 0 {
			return Definition{}, fault.New(fault.InvalidInput, "selected pipeline has no yaml")
		}
		selected.YAML = append([]byte(nil), selected.YAML...)
		return selected, nil
	}
	return Definition{}, fault.New(fault.InvalidInput, "admission requires a stored or inline definition")
}

func (service *AdmissionService) admitActions(ctx context.Context, pipeline definition.Pipeline, defaultTimeout time.Duration) ([]AdmittedAction, []PinnedImage, []id.SecretName, error) {
	actions := make([]AdmittedAction, 0, len(pipeline.Actions))
	images := make([]PinnedImage, 0)
	secretNames := make([]id.SecretName, 0)
	secretSeen := make(map[id.SecretName]bool)
	for index, action := range pipeline.Actions {
		timeout, err := resolveActionTimeout(action.Timeout, defaultTimeout)
		if err != nil {
			return nil, nil, nil, err
		}
		admitted := AdmittedAction{Index: index, Name: action.Name, Timeout: timeout}
		switch {
		case action.Container != nil:
			container := cloneContainer(*action.Container)
			if container.Image == "" {
				return nil, nil, nil, fault.New(fault.InvalidInput, fmt.Sprintf("action %q container image is empty", action.Name))
			}
			if _, err := script.Render(container.Commands); err != nil {
				return nil, nil, nil, fault.Wrap(fault.InvalidInput, fmt.Sprintf("action %q container commands are invalid", action.Name), err)
			}
			image := container.Image
			resolved, err := service.resolver.Resolve(ctx, ResolveRequest{Reference: image})
			if err != nil {
				return nil, nil, nil, resolverFault(action.Name, err)
			}
			if err := resolved.Validate(); err != nil {
				return nil, nil, nil, fault.Wrap(fault.UnsupportedCapability, fmt.Sprintf("image %q did not resolve to an immutable digest", image), err)
			}
			container.Image = repositoryOf(image) + "@" + resolved.Digest
			admitted.Kind = definition.ActionKindContainer
			admitted.Container = &container
			images = append(images, PinnedImage{Reference: image, Digest: resolved.Digest})
			for _, secret := range container.Secrets {
				if secretSeen[secret] {
					continue
				}
				secretSeen[secret] = true
				secretNames = append(secretNames, secret)
			}
		case action.Request != nil:
			request := cloneRequest(*action.Request)
			// A request action without an assignment or subject can never be
			// activated into a verifiable unit. Authoring refuses both shapes,
			// but admission refuses them again so a stored or inline definition
			// cannot slip past the authoring boundary.
			if request.Assignment == "" {
				return nil, nil, nil, fault.New(fault.InvalidInput, fmt.Sprintf("action %q request assignment is empty", action.Name))
			}
			if request.Subject.Len() == 0 {
				return nil, nil, nil, fault.New(fault.InvalidInput, fmt.Sprintf("action %q request has no subject", action.Name))
			}
			admitted.Kind = definition.ActionKindRequest
			admitted.Request = &request
		default:
			return nil, nil, nil, fault.New(fault.InvalidInput, fmt.Sprintf("action %q has no kind", action.Name))
		}
		if err := admitted.Validate(); err != nil {
			return nil, nil, nil, err
		}
		actions = append(actions, admitted)
	}
	return actions, images, secretNames, nil
}

func resolveActionTimeout(explicit, fallback time.Duration) (time.Duration, error) {
	if explicit != 0 {
		if explicit < minActionTimeout || explicit > maxActionTimeout {
			return 0, fault.New(fault.InvalidInput, fmt.Sprintf("action timeout %s is outside [%s, %s]", explicit, minActionTimeout, maxActionTimeout))
		}
		return explicit, nil
	}
	if fallback < minActionTimeout || fallback > maxActionTimeout {
		return 0, fault.New(fault.InvalidInput, fmt.Sprintf("default timeout %s is outside [%s, %s]", fallback, minActionTimeout, maxActionTimeout))
	}
	return fallback, nil
}

// resolverFault classifies one resolver failure. A reference the resolver
// cannot read as an image name is the caller's input and is refused for good;
// every other failure is an unavailable registry, which a caller may retry.
func resolverFault(action id.ActionName, err error) error {
	if class, ok := fault.ClassOf(err); ok && class == fault.DependencyUnavailable {
		return err
	}
	if errors.Is(err, ErrImageReference) {
		detail, ok := fault.MessageOf(err)
		if !ok {
			detail = ErrImageReference.Error()
		}
		return fault.Wrap(fault.InvalidInput, fmt.Sprintf("action %q image is not usable: %s", action, detail), err)
	}
	return fault.Wrap(fault.DependencyUnavailable, "image registry is unavailable", err)
}

func repositoryOf(reference string) string {
	if before, _, ok := strings.Cut(reference, "@"); ok {
		return before
	}
	slash := strings.LastIndex(reference, "/")
	colon := strings.LastIndex(reference, ":")
	if colon > slash {
		return reference[:colon]
	}
	return reference
}

func cloneContainer(container definition.Container) definition.Container {
	cloned := container
	cloned.Commands = append([]string(nil), container.Commands...)
	cloned.Env = append([]definition.EnvVar(nil), container.Env...)
	cloned.Secrets = append([]id.SecretName(nil), container.Secrets...)
	cloned.Outputs = append([]id.OutputKey(nil), container.Outputs...)
	return cloned
}

func cloneRequest(request definition.Request) definition.Request {
	cloned := request
	cloned.Subject = definition.NewSubject(request.Subject.Fields())
	cloned.Outputs = append([]id.OutputKey(nil), request.Outputs...)
	return cloned
}

func validateTaskContext(task TaskContext) (map[string]string, error) {
	if task.TaskID != "" {
		if _, err := id.ParseTaskID(string(task.TaskID)); err != nil {
			return nil, fault.Wrap(fault.InvalidInput, "task id is invalid", err)
		}
	}
	if len(task.Conversation) > maxTaskConversationBytes {
		return nil, fault.New(fault.InvalidInput, fmt.Sprintf("task conversation is %d bytes; at most %d are allowed", len(task.Conversation), maxTaskConversationBytes))
	}
	metadata, err := parseTaskMetadata(task.Metadata)
	if err != nil {
		return nil, err
	}
	return metadata, nil
}

func parseTaskMetadata(raw []byte) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) > maxTaskMetadataBytes {
		return nil, fault.New(fault.InvalidInput, fmt.Sprintf("task metadata is %d bytes; at most %d are allowed", len(raw), maxTaskMetadataBytes))
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		return nil, fault.Wrap(fault.InvalidInput, "task metadata must be a JSON object of string values", err)
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return nil, fault.New(fault.InvalidInput, "task metadata must be a JSON object of string values")
	}
	metadata := make(map[string]string)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, fault.Wrap(fault.InvalidInput, "task metadata must be a JSON object of string values", err)
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, fault.New(fault.InvalidInput, "task metadata must be a JSON object of string values")
		}
		if _, duplicate := metadata[key]; duplicate {
			return nil, fault.New(fault.InvalidInput, fmt.Sprintf("task metadata key %q is duplicated", key))
		}
		valueToken, err := decoder.Token()
		if err != nil {
			return nil, fault.Wrap(fault.InvalidInput, "task metadata must be a JSON object of string values", err)
		}
		value, ok := valueToken.(string)
		if !ok {
			return nil, fault.New(fault.InvalidInput, fmt.Sprintf("task metadata value for %q must be a string", key))
		}
		metadata[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, fault.Wrap(fault.InvalidInput, "task metadata must be a JSON object of string values", err)
	}
	if token, err := decoder.Token(); err != io.EOF {
		if err != nil {
			return nil, fault.Wrap(fault.InvalidInput, "task metadata must be a JSON object of string values", err)
		}
		_ = token
		return nil, fault.New(fault.InvalidInput, "task metadata has more than one JSON value")
	}
	return metadata, nil
}
