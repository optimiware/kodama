package api

import "time"

// Secret metadata and write wire vocabulary.
//
// A secret write carries the caller-supplied value once; no read result ever
// carries a secret value. The metadata read exposes identity, name, version and
// update time only.

// SecretMetadataReadRequest is the secret.metadata.read input. The project and
// secret are carried explicitly for MCP callers and are optional for REST
// callers whose route supplies them.
type SecretMetadataReadRequest struct {
	ProjectID string `json:"project_id,omitempty" schema:"id"`
	SecretID  string `json:"secret_id,omitempty" schema:"id"`
}

// Validate checks the optional project and secret identities.
func (request SecretMetadataReadRequest) Validate() error {
	if request.ProjectID != "" && !validID(request.ProjectID) {
		return invalid("project_id is invalid")
	}
	if request.SecretID != "" && !validID(request.SecretID) {
		return invalid("secret_id is invalid")
	}
	return nil
}

// SecretMetadataReadResult is the secret.metadata.read result.
type SecretMetadataReadResult struct {
	SecretID  string    `json:"secret_id"`
	Name      string    `json:"name"`
	Version   uint64    `json:"version" schema:"version"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Validate checks the secret identity, name, version and update time.
func (result SecretMetadataReadResult) Validate() error {
	if !validID(result.SecretID) {
		return invalid("secret_id is invalid")
	}
	if result.Name == "" {
		return invalid("secret name is empty")
	}
	if result.Version == 0 {
		return invalid("secret version must be positive")
	}
	if result.UpdatedAt.IsZero() {
		return invalid("secret updated_at is zero")
	}
	return nil
}

// SecretWriteRequest is the secret.write input. The project and secret are
// carried explicitly for MCP callers and are optional for REST callers whose
// route supplies them.
type SecretWriteRequest struct {
	ProjectID string `json:"project_id,omitempty" schema:"id"`
	SecretID  string `json:"secret_id,omitempty" schema:"id"`
	Value     string `json:"value" schema:"secret"`
}

// Validate checks the optional project/secret identities and the non-empty
// 64 KiB secret value.
func (request SecretWriteRequest) Validate() error {
	if request.ProjectID != "" && !validID(request.ProjectID) {
		return invalid("project_id is invalid")
	}
	if request.SecretID != "" && !validID(request.SecretID) {
		return invalid("secret_id is invalid")
	}
	if request.Value == "" {
		return invalid("secret value is empty")
	}
	if len(request.Value) > MaxEventDataBytes {
		return invalid("secret value is %d bytes; at most %d are allowed", len(request.Value), MaxEventDataBytes)
	}
	return nil
}

// SecretWriteResult is the secret.write result.
type SecretWriteResult struct {
	SecretID string `json:"secret_id"`
	Name     string `json:"name"`
	Version  uint64 `json:"version" schema:"version"`
}

// Validate checks the secret identity, name and committed version.
func (result SecretWriteResult) Validate() error {
	if !validID(result.SecretID) {
		return invalid("secret_id is invalid")
	}
	if result.Name == "" {
		return invalid("secret name is empty")
	}
	if result.Version == 0 {
		return invalid("secret version must be positive")
	}
	return nil
}

// SecretDeleteRequest is the secret.delete input. The project and secret are
// carried explicitly for MCP callers and are optional for REST callers whose
// route supplies them.
type SecretDeleteRequest struct {
	ProjectID string `json:"project_id,omitempty" schema:"id"`
	SecretID  string `json:"secret_id,omitempty" schema:"id"`
}

// Validate checks the optional project and secret identities.
func (request SecretDeleteRequest) Validate() error {
	if request.ProjectID != "" && !validID(request.ProjectID) {
		return invalid("project_id is invalid")
	}
	if request.SecretID != "" && !validID(request.SecretID) {
		return invalid("secret_id is invalid")
	}
	return nil
}

// SecretDeleteResult is the secret.delete result.
type SecretDeleteResult struct {
	Version uint64 `json:"version" schema:"version"`
}

// Validate checks the committed version.
func (result SecretDeleteResult) Validate() error {
	if result.Version == 0 {
		return invalid("secret version must be positive")
	}
	return nil
}
