package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Pipeline and immutable execution-start wire vocabulary.
//
// A pipeline document is bounded YAML bytes; admission is a separate Shell
// decision. The public DTO never carries an admitted definition or execution
// plan.

// PipelineApplyRequest is the pipeline.apply input. The project is carried
// explicitly for MCP callers and is optional for REST callers whose route
// supplies it. The pipeline route segment is the pipeline NAME, not a stored
// pipeline id: apply creates or replaces a same-name pipeline.
type PipelineApplyRequest struct {
	ProjectID string `json:"project_id,omitempty" schema:"id"`
	Name      string `json:"name"`
	YAML      []byte `json:"yaml" schema:"yaml"`
}

// Validate checks the optional project identity, the domain pipeline-name
// grammar and the non-empty bounded YAML document.
func (request PipelineApplyRequest) Validate() error {
	if request.ProjectID != "" && !validID(request.ProjectID) {
		return invalid("project_id is invalid")
	}
	if !validPipelineName(request.Name) {
		return invalid("pipeline name %q is invalid", request.Name)
	}
	return validatePipelineYAML(request.YAML)
}

// validPipelineName mirrors the domain validatePipelineName grammar: a
// non-empty bounded name of [A-Za-z0-9._-], excluding "." and "..".
func validPipelineName(name string) bool {
	if name == "" || len(name) > maxIDLength || name == "." || name == ".." {
		return false
	}
	for index := 0; index < len(name); index++ {
		character := name[index]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '-', character == '_', character == '.':
		default:
			return false
		}
	}
	return true
}

// PipelineApplyResult is the pipeline.apply result.
type PipelineApplyResult struct {
	PipelineID string `json:"pipeline_id"`
	Version    uint64 `json:"version" schema:"version"`
}

// Validate checks the pipeline identity and committed version.
func (result PipelineApplyResult) Validate() error {
	if !validID(result.PipelineID) {
		return invalid("pipeline_id is invalid")
	}
	if result.Version == 0 {
		return invalid("pipeline version must be positive")
	}
	return nil
}

// PipelineReadRequest is the pipeline.read input. The project and pipeline are
// carried explicitly for MCP callers and are optional for REST callers whose
// route supplies them.
type PipelineReadRequest struct {
	ProjectID  string `json:"project_id,omitempty" schema:"id"`
	PipelineID string `json:"pipeline_id,omitempty" schema:"id"`
}

// Validate checks the optional project and pipeline identities.
func (request PipelineReadRequest) Validate() error {
	if request.ProjectID != "" && !validID(request.ProjectID) {
		return invalid("project_id is invalid")
	}
	if request.PipelineID != "" && !validID(request.PipelineID) {
		return invalid("pipeline_id is invalid")
	}
	return nil
}

// PipelineReadResult is the pipeline.read result.
type PipelineReadResult struct {
	PipelineID string `json:"pipeline_id"`
	Name       string `json:"name"`
	Version    uint64 `json:"version" schema:"version"`
	YAML       []byte `json:"yaml" schema:"yaml"`
}

// Validate checks the pipeline identity, name, committed version and bounded
// YAML document.
func (result PipelineReadResult) Validate() error {
	if !validID(result.PipelineID) {
		return invalid("pipeline_id is invalid")
	}
	if result.Name == "" {
		return invalid("pipeline name is empty")
	}
	if result.Version == 0 {
		return invalid("pipeline version must be positive")
	}
	return validatePipelineYAML(result.YAML)
}

// PipelineUpdateRequest is the pipeline.update input. The project and pipeline
// are carried explicitly for MCP callers and are optional for REST callers
// whose route supplies them.
type PipelineUpdateRequest struct {
	ProjectID  string `json:"project_id,omitempty" schema:"id"`
	PipelineID string `json:"pipeline_id,omitempty" schema:"id"`
	Name       string `json:"name"`
	YAML       []byte `json:"yaml" schema:"yaml"`
}

// Validate checks the optional project/pipeline identities, non-empty name and
// bounded YAML document.
func (request PipelineUpdateRequest) Validate() error {
	if request.ProjectID != "" && !validID(request.ProjectID) {
		return invalid("project_id is invalid")
	}
	if request.PipelineID != "" && !validID(request.PipelineID) {
		return invalid("pipeline_id is invalid")
	}
	if request.Name == "" {
		return invalid("pipeline name is empty")
	}
	return validatePipelineYAML(request.YAML)
}

// PipelineUpdateResult is the pipeline.update result.
type PipelineUpdateResult struct {
	Version uint64 `json:"version" schema:"version"`
}

// Validate checks the committed version.
func (result PipelineUpdateResult) Validate() error {
	if result.Version == 0 {
		return invalid("pipeline version must be positive")
	}
	return nil
}

// PipelineDeleteRequest is the pipeline.delete input. The project and pipeline
// are carried explicitly for MCP callers and are optional for REST callers
// whose route supplies them.
type PipelineDeleteRequest struct {
	ProjectID  string `json:"project_id,omitempty" schema:"id"`
	PipelineID string `json:"pipeline_id,omitempty" schema:"id"`
}

// Validate checks the optional project and pipeline identities.
func (request PipelineDeleteRequest) Validate() error {
	if request.ProjectID != "" && !validID(request.ProjectID) {
		return invalid("project_id is invalid")
	}
	if request.PipelineID != "" && !validID(request.PipelineID) {
		return invalid("pipeline_id is invalid")
	}
	return nil
}

// PipelineDeleteResult is the pipeline.delete result.
type PipelineDeleteResult struct {
	Version uint64 `json:"version" schema:"version"`
}

// Validate checks the committed version.
func (result PipelineDeleteResult) Validate() error {
	if result.Version == 0 {
		return invalid("pipeline version must be positive")
	}
	return nil
}

// PipelineStartRequest is the pipeline.start input. The project is carried
// explicitly for MCP callers and is optional for REST callers whose route
// supplies it. Exactly one selection is required: a stored pipeline id (with
// the expected pipeline version carried by the shared envelope) or an inline
// YAML document on the task-free executions route. An inline document is
// admitted with no stored pipeline row and is identified by its exact bytes.
type PipelineStartRequest struct {
	ProjectID  string            `json:"project_id,omitempty" schema:"id"`
	PipelineID string            `json:"pipeline_id,omitempty" schema:"id"`
	YAML       []byte            `json:"yaml,omitempty" schema:"yaml"`
	Inputs     map[string]string `json:"inputs,omitempty"`
}

// Validate checks the optional project identity, the exclusive stored/inline
// selection, the pipeline identity grammar, the bounded inline YAML document
// and the start inputs.
func (request PipelineStartRequest) Validate() error {
	if request.ProjectID != "" && !validID(request.ProjectID) {
		return invalid("project_id is invalid")
	}
	stored := request.PipelineID != ""
	inline := len(request.YAML) > 0
	switch {
	case stored && inline:
		return invalid("pipeline.start accepts exactly one of pipeline_id or yaml; both were supplied")
	case !stored && !inline:
		return invalid("pipeline.start requires exactly one of pipeline_id or yaml")
	case stored:
		if !validID(request.PipelineID) {
			return invalid("pipeline_id is invalid")
		}
	}
	if inline {
		if err := validatePipelineYAML(request.YAML); err != nil {
			return err
		}
	}
	return validateInputs(request.Inputs)
}

// PipelineStartIdempotencyContent returns the canonical receipt content of one
// pipeline.start request. A stored selection contributes the pipeline id and
// the expected pipeline version carried by the envelope; an inline selection
// contributes only the exact document's SHA-256, never the document or its
// base64. Inputs are always present (an absent map equals an empty one), so an
// equal retry with the same bytes and inputs produces byte-identical content.
func PipelineStartIdempotencyContent(request PipelineStartRequest, expectedVersion *uint64) ([]byte, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	inputs := request.Inputs
	if inputs == nil {
		inputs = map[string]string{}
	}
	if len(request.YAML) > 0 {
		if expectedVersion != nil {
			return nil, invalid("inline pipeline yaml does not accept an expected pipeline version")
		}
		digest := sha256.Sum256(request.YAML)
		return json.Marshal(struct {
			YAMLSHA256 string            `json:"yaml_sha256"`
			Inputs     map[string]string `json:"inputs"`
		}{
			YAMLSHA256: "sha256:" + hex.EncodeToString(digest[:]),
			Inputs:     inputs,
		})
	}
	if expectedVersion == nil {
		return nil, invalid("stored pipeline selection requires an expected pipeline version")
	}
	if *expectedVersion == 0 {
		return nil, invalid("expected pipeline version must be positive")
	}
	return json.Marshal(struct {
		PipelineID      string            `json:"pipeline_id"`
		PipelineVersion uint64            `json:"pipeline_version"`
		Inputs          map[string]string `json:"inputs"`
	}{
		PipelineID:      request.PipelineID,
		PipelineVersion: *expectedVersion,
		Inputs:          inputs,
	})
}

// PipelineStartResult is the pipeline.start result. It carries the admitted
// execution identity and the EXECUTION version, never a pipeline identity or
// pipeline version.
type PipelineStartResult struct {
	ExecutionID string `json:"execution_id"`
	Version     uint64 `json:"version" schema:"version"`
}

// Validate checks the admitted execution identity and version.
func (result PipelineStartResult) Validate() error {
	if !validID(result.ExecutionID) {
		return invalid("execution_id is invalid")
	}
	if result.Version == 0 {
		return invalid("execution version must be positive")
	}
	return nil
}

// PipelineListRequest is the pipeline.list input. The project is carried
// explicitly for MCP callers and is optional for REST callers whose route
// supplies it.
type PipelineListRequest struct {
	ProjectID string `json:"project_id,omitempty" schema:"id"`
	Page      *Page  `json:"page,omitempty"`
}

// Validate checks the optional project identity and page bounds.
func (request PipelineListRequest) Validate() error {
	if request.ProjectID != "" && !validID(request.ProjectID) {
		return invalid("project_id is invalid")
	}
	if request.Page != nil {
		return request.Page.Validate()
	}
	return nil
}

// PipelineSummaryView is one pipeline summary observation in pipeline.list.
type PipelineSummaryView struct {
	PipelineID string `json:"pipeline_id"`
	Name       string `json:"name"`
	Version    uint64 `json:"version" schema:"version"`
}

// Validate checks the pipeline identity, non-empty name and version.
func (view PipelineSummaryView) Validate() error {
	if !validID(view.PipelineID) {
		return invalid("pipeline_id is invalid")
	}
	if !validPipelineName(view.Name) {
		return invalid("pipeline name %q is invalid", view.Name)
	}
	if view.Version == 0 {
		return invalid("pipeline version must be positive")
	}
	return nil
}

// PipelineListResult is the pipeline.list result.
type PipelineListResult struct {
	Pipelines  []PipelineSummaryView `json:"pipelines"`
	NextCursor string                `json:"next_cursor,omitempty"`
}

// Validate checks every pipeline summary and the optional cursor bound.
func (result PipelineListResult) Validate() error {
	for _, pipeline := range result.Pipelines {
		if err := pipeline.Validate(); err != nil {
			return err
		}
	}
	if len(result.NextCursor) > MaxCursorBytes {
		return invalid("next_cursor is %d bytes; at most %d are allowed", len(result.NextCursor), MaxCursorBytes)
	}
	return nil
}

// validatePipelineYAML checks the 1 MiB YAML document bound.
func validatePipelineYAML(document []byte) error {
	if len(document) == 0 {
		return invalid("pipeline yaml is empty")
	}
	if len(document) > MaxInlineYAMLBytes {
		return invalid("pipeline yaml is %d bytes; at most %d are allowed", len(document), MaxInlineYAMLBytes)
	}
	return nil
}

// validateInputs checks an optional bounded start input map.
func validateInputs(inputs map[string]string) error {
	for key, value := range inputs {
		if key == "" {
			return invalid("pipeline input name is empty")
		}
		if len(key) > maxIDLength {
			return invalid("pipeline input name is too long")
		}
		if len(value) > MaxRequestBytes {
			return invalid("pipeline input %q value is too long", key)
		}
	}
	return nil
}
