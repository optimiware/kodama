package productmcp

// Tool inputs and outputs. They are the MCP wire shapes of the tools: simple
// types only (RFC 3339 strings, no custom marshalers) so the SDK derives their
// JSON Schema. Every creating tool takes an idempotencyKey.

// ProjectCreateIn creates a project in the caller's tenant.
type ProjectCreateIn struct {
	IdempotencyKey string `json:"idempotencyKey" jsonschema:"Client-chosen key (1-64 characters from A-Z a-z 0-9 . _ ~ : -). Repeating the call with the same key and arguments returns the original project; the same key with different arguments is refused with idempotency_conflict."`
	Key            string `json:"key" jsonschema:"Stable project key: a letter or digit followed by up to 62 letters, digits, underscores or hyphens."`
	DisplayName    string `json:"displayName,omitempty" jsonschema:"Human-readable name, at most 200 characters."`
	ActiveWIPLimit int    `json:"activeWipLimit,omitempty" jsonschema:"Maximum active work items; omit for the default."`
}

// ProjectGetIn reads one project.
type ProjectGetIn struct {
	ProjectID string `json:"projectId" jsonschema:"The project identifier."`
}

// ProjectListIn lists the projects the caller may read.
type ProjectListIn struct {
	Cursor *string `json:"cursor,omitempty" jsonschema:"The nextCursor of the previous page."`
}

// ProjectOut is a project.
type ProjectOut struct {
	ID             string `json:"id"`
	Key            string `json:"key"`
	DisplayName    string `json:"displayName"`
	ActiveWIPLimit int    `json:"activeWipLimit"`
	Version        int64  `json:"version"`
	// Replayed is true when the call returned the project of an earlier call
	// with the same idempotency key.
	Replayed bool `json:"replayed,omitempty"`
}

// ProjectListOut is one page of projects.
type ProjectListOut struct {
	Items      []ProjectOut `json:"items"`
	NextCursor *string      `json:"nextCursor"`
}

// RepositoryCreateIn creates an internally hosted repository in a project.
type RepositoryCreateIn struct {
	ProjectID           string  `json:"projectId,omitempty" jsonschema:"The project. A provisioning attempt may omit it and acts only in its run's project."`
	IdempotencyKey      string  `json:"idempotencyKey" jsonschema:"Client-chosen key (1-64 characters from A-Z a-z 0-9 . _ ~ : -). Repeating the call with the same key and arguments returns the original repository; the same key with different arguments is refused with idempotency_conflict."`
	Name                string  `json:"name" jsonschema:"Project-unique name matching ^[a-z][a-z0-9-]{0,62}$."`
	DefaultBranch       *string `json:"defaultBranch,omitempty" jsonschema:"Default branch name, not a ref path; main when omitted."`
	DirectPushToDefault *bool   `json:"directPushToDefault,omitempty" jsonschema:"Allow engineers to push the default branch directly after seeding; false when omitted."`
}

// RepositoryGetIn reads one repository.
type RepositoryGetIn struct {
	ProjectID    string `json:"projectId,omitempty" jsonschema:"The project. A provisioning attempt may omit it."`
	RepositoryID string `json:"repositoryId" jsonschema:"The repository identifier."`
}

// RepositoryListIn lists a project's repositories.
type RepositoryListIn struct {
	ProjectID string  `json:"projectId,omitempty" jsonschema:"The project. A provisioning attempt may omit it."`
	Cursor    *string `json:"cursor,omitempty" jsonschema:"The nextCursor of the previous page."`
}

// RepositoryOut is a repository. CloneURL never carries credentials.
type RepositoryOut struct {
	ID                  string  `json:"id"`
	Name                string  `json:"name"`
	DefaultBranch       string  `json:"defaultBranch"`
	Hosting             string  `json:"hosting"`
	Status              string  `json:"status"`
	Reason              *string `json:"reason"`
	CloneURL            string  `json:"cloneUrl"`
	DirectPushToDefault bool    `json:"directPushToDefault"`
	Empty               bool    `json:"empty"`
	Version             int64   `json:"version"`
	CreatedAt           string  `json:"createdAt"`
	Replayed            bool    `json:"replayed,omitempty"`
}

// RepositoryListOut is one page of repositories.
type RepositoryListOut struct {
	Items      []RepositoryOut `json:"items"`
	NextCursor *string         `json:"nextCursor"`
}

// SeedIn requests a seed grant for an empty repository.
type SeedIn struct {
	ProjectID    string `json:"projectId" jsonschema:"The project."`
	RepositoryID string `json:"repositoryId" jsonschema:"The empty, ready repository to seed."`
}

// SeedOut tells the engineer how to push. Password is the grant's one-time
// credential: it is shown only here, is never stored by this server beyond its
// hash and never appears in a log. It expires after at most 15 minutes and
// authorizes exactly one receive that creates the default branch.
type SeedOut struct {
	RepositoryID  string   `json:"repositoryId"`
	CloneURL      string   `json:"cloneUrl"`
	DefaultBranch string   `json:"defaultBranch"`
	Username      string   `json:"username"`
	Password      string   `json:"password"`
	GrantID       string   `json:"grantId"`
	ExpiresAt     string   `json:"expiresAt"`
	Instructions  []string `json:"instructions"`
}

// WorkItemCriterion is one acceptance criterion.
type WorkItemCriterion struct {
	ID   string `json:"id" jsonschema:"Criterion identifier unique within the item."`
	Text string `json:"text" jsonschema:"The criterion."`
}

// WorkItemCreateIn creates a work item (a task by default) in backlog.
type WorkItemCreateIn struct {
	ProjectID          string              `json:"projectId,omitempty" jsonschema:"The project. A provisioning attempt may omit it."`
	IdempotencyKey     string              `json:"idempotencyKey" jsonschema:"Client-chosen key (1-64 characters from A-Z a-z 0-9 . _ ~ : -). Repeating the call with the same key and arguments returns the original work item; the same key with different arguments is refused with idempotency_conflict."`
	Kind               string              `json:"kind,omitempty" jsonschema:"epic, story, task or defect; task when omitted."`
	Title              string              `json:"title" jsonschema:"Short title, at most 200 characters."`
	Spec               string              `json:"spec,omitempty" jsonschema:"The specification text."`
	AcceptanceCriteria []WorkItemCriterion `json:"acceptanceCriteria,omitempty" jsonschema:"Structured acceptance criteria."`
}

// WorkItemGetIn reads one work item.
type WorkItemGetIn struct {
	ProjectID  string `json:"projectId,omitempty" jsonschema:"The project. A provisioning attempt may omit it."`
	WorkItemID string `json:"workItemId" jsonschema:"The work item identifier."`
}

// WorkItemListIn lists a project's work items.
type WorkItemListIn struct {
	ProjectID string   `json:"projectId,omitempty" jsonschema:"The project. A provisioning attempt may omit it."`
	States    []string `json:"states,omitempty" jsonschema:"Only items in these states."`
	Kinds     []string `json:"kinds,omitempty" jsonschema:"Only items of these kinds."`
	Cursor    *string  `json:"cursor,omitempty" jsonschema:"The nextCursor of the previous page."`
	Limit     int      `json:"limit,omitempty" jsonschema:"Page size, 1 to 100; 50 when omitted."`
}

// WorkItemOut is a work item.
type WorkItemOut struct {
	ID              string `json:"id"`
	Kind            string `json:"kind"`
	Title           string `json:"title"`
	State           string `json:"state"`
	Version         int64  `json:"version"`
	ContentRevision int64  `json:"contentRevision"`
	CreatedAt       string `json:"createdAt"`
	Replayed        bool   `json:"replayed,omitempty"`
}

// WorkItemListOut is one page of work items.
type WorkItemListOut struct {
	Items      []WorkItemOut `json:"items"`
	NextCursor *string       `json:"nextCursor"`
}

// ProvisioningStartIn starts a provisioning run within a project, bounded by
// the server's input and active-run limits.
type ProvisioningStartIn struct {
	ProjectID      string            `json:"projectId" jsonschema:"The project the run is admitted in; the run acts only there."`
	IdempotencyKey string            `json:"idempotencyKey" jsonschema:"Client-chosen key (1-64 characters from A-Z a-z 0-9 . _ ~ : -). Repeating the call with the same key and arguments returns the original run; the same key with different arguments is refused with idempotency_conflict."`
	DefinitionID   string            `json:"definitionId" jsonschema:"The pipeline definition to run."`
	Repository     string            `json:"repository,omitempty" jsonschema:"Name of an existing repository of the project for the run's implicit source; omit for a source-free run."`
	Inputs         map[string]string `json:"inputs,omitempty" jsonschema:"Run inputs as strings; bounded in count and size."`
}

// RunOut is a provisioning run. The run receives its seed authority through the
// runtime route for repositories it creates itself; this tool returns no credential.
type RunOut struct {
	ID           string `json:"id"`
	DefinitionID string `json:"definitionId"`
	Status       string `json:"status"`
	Reason       string `json:"reason,omitempty"`
	Replayed     bool   `json:"replayed,omitempty"`
}
