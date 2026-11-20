package api

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Repository onboarding and discovery wire vocabulary.
//
// A repository operation coordinates Git companion provisioning and the
// project's Git resource binding while keeping Git storage ownership and
// access-rule evaluation inside the independent Userland Git service.

// DefaultRepositoryBranch is the default initial branch when a caller omits
// default_branch on repository.create.
const DefaultRepositoryBranch = "main"

// RepositoryCreateRequest is the repository.create input. The project is
// carried explicitly for MCP callers and is optional for REST callers whose
// route supplies it.
type RepositoryCreateRequest struct {
	ProjectID        string            `json:"project_id,omitempty" schema:"id"`
	Name             string            `json:"name"`
	DefaultBranch    string            `json:"default_branch,omitempty"`
	ResourceID       string            `json:"resource_id,omitempty" schema:"id"`
	BindingID        string            `json:"binding_id,omitempty" schema:"id"`
	EnforcingService string            `json:"enforcing_service,omitempty" schema:"id"`
	TrustedOrigins   []string          `json:"trusted_origins,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

// Validate checks the optional project/resource/binding/service identities, the
// tenant-relative repository name, optional branch name, trusted origins and
// bounded metadata.
func (request RepositoryCreateRequest) Validate() error {
	if request.ProjectID != "" && !validID(request.ProjectID) {
		return invalid("project_id is invalid")
	}
	if !ValidRepositoryName(request.Name) {
		return invalid("repository name %q is invalid", request.Name)
	}
	if request.Name == "admin" || strings.HasPrefix(request.Name, "admin/") {
		return invalid("repository name %q is reserved", request.Name)
	}
	if request.DefaultBranch != "" && !ValidBranchName(request.DefaultBranch) {
		return invalid("default_branch %q is invalid", request.DefaultBranch)
	}
	if request.ResourceID != "" && !validID(request.ResourceID) {
		return invalid("resource_id is invalid")
	}
	if request.BindingID != "" && !validID(request.BindingID) {
		return invalid("binding_id is invalid")
	}
	if request.EnforcingService != "" && !validID(request.EnforcingService) {
		return invalid("enforcing_service is invalid")
	}
	if request.TrustedOrigins != nil {
		if err := validateTrustedOrigins(request.TrustedOrigins); err != nil {
			return err
		}
	}
	return validateBindingMetadata(request.Metadata)
}

// RepositoryCreateResult is the repository.create result.
type RepositoryCreateResult struct {
	Name             string            `json:"name"`
	ResourceID       string            `json:"resource_id"`
	BindingID        string            `json:"binding_id"`
	EnforcingService string            `json:"enforcing_service"`
	DefaultBranch    string            `json:"default_branch"`
	CloneURL         string            `json:"clone_url,omitempty"`
	TrustedOrigins   []string          `json:"trusted_origins"`
	Version          uint64            `json:"version" schema:"version"`
	Created          bool              `json:"created"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

// Validate checks the repository identity, resource binding fields and version.
func (result RepositoryCreateResult) Validate() error {
	if !ValidRepositoryName(result.Name) {
		return invalid("repository name %q is invalid", result.Name)
	}
	if !validID(result.ResourceID) {
		return invalid("resource_id is invalid")
	}
	if !validID(result.BindingID) {
		return invalid("binding_id is invalid")
	}
	if !validID(result.EnforcingService) {
		return invalid("enforcing_service is invalid")
	}
	if !ValidBranchName(result.DefaultBranch) {
		return invalid("default_branch %q is invalid", result.DefaultBranch)
	}
	if err := validateTrustedOrigins(result.TrustedOrigins); err != nil {
		return err
	}
	if err := validateBindingMetadata(result.Metadata); err != nil {
		return err
	}
	if result.Version == 0 {
		return invalid("repository binding version must be positive")
	}
	return nil
}

// RepositoryListRequest is the repository.list input. The project is carried
// explicitly for MCP callers and is optional for REST callers whose route
// supplies it.
type RepositoryListRequest struct {
	ProjectID string `json:"project_id,omitempty" schema:"id"`
	Page      *Page  `json:"page,omitempty"`
}

// Validate checks the optional project identity and page bounds.
func (request RepositoryListRequest) Validate() error {
	if request.ProjectID != "" && !validID(request.ProjectID) {
		return invalid("project_id is invalid")
	}
	if request.Page != nil {
		return request.Page.Validate()
	}
	return nil
}

// RepositoryView is one observed Git repository in repository.list.
type RepositoryView struct {
	Name             string            `json:"name"`
	ResourceID       string            `json:"resource_id"`
	BindingID        string            `json:"binding_id"`
	EnforcingService string            `json:"enforcing_service"`
	DefaultBranch    string            `json:"default_branch,omitempty"`
	CloneURL         string            `json:"clone_url,omitempty"`
	TrustedOrigins   []string          `json:"trusted_origins"`
	Version          uint64            `json:"version" schema:"version"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

// Validate checks every observed repository member.
func (view RepositoryView) Validate() error {
	if view.Name == "" || len(view.Name) > maxIDLength {
		return invalid("repository name is invalid")
	}
	if !validID(view.ResourceID) {
		return invalid("resource_id is invalid")
	}
	if !validID(view.BindingID) {
		return invalid("binding_id is invalid")
	}
	if !validID(view.EnforcingService) {
		return invalid("enforcing_service is invalid")
	}
	if view.DefaultBranch != "" && !ValidBranchName(view.DefaultBranch) {
		return invalid("default_branch %q is invalid", view.DefaultBranch)
	}
	if err := validateTrustedOrigins(view.TrustedOrigins); err != nil {
		return err
	}
	if err := validateBindingMetadata(view.Metadata); err != nil {
		return err
	}
	if view.Version == 0 {
		return invalid("repository binding version must be positive")
	}
	return nil
}

// RepositoryListResult is the repository.list result.
type RepositoryListResult struct {
	Repositories []RepositoryView `json:"repositories"`
	NextCursor   string           `json:"next_cursor,omitempty"`
}

// Validate checks every repository view and the optional cursor bound.
func (result RepositoryListResult) Validate() error {
	for _, repository := range result.Repositories {
		if err := repository.Validate(); err != nil {
			return err
		}
	}
	if len(result.NextCursor) > MaxCursorBytes {
		return invalid("next_cursor is %d bytes; at most %d are allowed", len(result.NextCursor), MaxCursorBytes)
	}
	return nil
}

// ValidRepositoryName reports whether value is a canonical tenant-relative Git
// repository name: slash-separated lowercase [a-z0-9._-] segments without dot
// segments, a trailing ".git" suffix, globs, whitespace or leading/trailing
// slashes.
func ValidRepositoryName(value string) bool {
	if value == "" || len(value) > maxIDLength {
		return false
	}
	if strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.HasSuffix(value, ".git") {
		return false
	}
	if strings.ContainsAny(value, "*?[]\\ ") || strings.Contains(value, "..") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
		for index := 0; index < len(segment); index++ {
			character := segment[index]
			switch {
			case character >= 'a' && character <= 'z':
			case character >= '0' && character <= '9':
			case character == '-', character == '_', character == '.':
			default:
				return false
			}
		}
	}
	return true
}

// ValidBranchName reports whether value is a safe, bounded Git branch name
// suitable for refs/heads/<branch>.
func ValidBranchName(value string) bool {
	if value == "" || len(value) > maxIDLength {
		return false
	}
	if strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.HasPrefix(value, "-") || strings.HasSuffix(value, ".") {
		return false
	}
	if strings.Contains(value, "..") || strings.Contains(value, "//") || strings.Contains(value, "@{") || strings.HasSuffix(value, ".lock") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." || strings.HasPrefix(segment, ".") || strings.HasSuffix(segment, ".lock") {
			return false
		}
		for index := 0; index < len(segment); index++ {
			character := segment[index]
			switch {
			case character >= 'a' && character <= 'z':
			case character >= 'A' && character <= 'Z':
			case character >= '0' && character <= '9':
			case character == '-', character == '_', character == '.':
			default:
				return false
			}
		}
	}
	return true
}

// DeriveRepositoryResourceID derives a deterministic valid resource_id
// ("git-" + normalized repository name, bounded to 128 bytes) when a caller
// omits resource_id on repository.create.
func DeriveRepositoryResourceID(repository string) string {
	var builder strings.Builder
	builder.WriteString("git-")
	for index := 0; index < len(repository); index++ {
		character := repository[index]
		switch {
		case character >= 'a' && character <= 'z':
			builder.WriteByte(character)
		case character >= 'A' && character <= 'Z':
			builder.WriteByte(character + ('a' - 'A'))
		case character >= '0' && character <= '9':
			builder.WriteByte(character)
		case character == '-', character == '_':
			builder.WriteByte(character)
		default:
			builder.WriteByte('-')
		}
	}
	candidate := builder.String()
	if len(candidate) <= maxIDLength && validID(candidate) {
		return candidate
	}
	sum := sha256.Sum256([]byte(repository))
	suffix := "-" + hex.EncodeToString(sum[:8])
	prefix := candidate
	if maxPrefix := maxIDLength - len(suffix); len(prefix) > maxPrefix {
		prefix = prefix[:maxPrefix]
	}
	return prefix + suffix
}
