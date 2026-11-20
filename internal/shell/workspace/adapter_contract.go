// Package workspace declares the activation-materialization seam and the
// confined path rules shared by every runtime adapter.
//
// Preparation writes an immutable activation snapshot outside the writable
// workspace, then attaches the execution's existing /workspace. Only four
// roots exist: admitted inputs, earlier succeeded outputs, the optional task
// snapshot and declared activation-time secrets. The execution credential is
// the sole mutable mounted file and is the only reserved credential path.
//
// Paths are absolute, confined, free of traversal and duplicate separators,
// and never inside /workspace. Partial preparation never authorizes a start:
// only a complete preparation with a stable workspace identity does.
package workspace

import (
	"context"
	"os"
	"strings"

	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/kernel/engine"
)

// Immutable preparation roots and the reserved credential mount path.
const (
	// InputRoot holds admitted input string bytes.
	InputRoot = "/kodema/inputs"
	// OutputRoot holds earlier succeeded action outputs.
	OutputRoot = "/kodema/outputs"
	// TaskPath is the admitted task conversation file. It is absent when task-free.
	TaskPath = "/kodema/task.json"
	// TaskRoot holds admitted read-only task specification files. It is absent
	// when task-free.
	TaskRoot = "/kodema/task"
	// TaskTitlePath is the admitted task title (UTF-8 text).
	TaskTitlePath = TaskRoot + "/title"
	// TaskDescriptionPath is the admitted task description (UTF-8 text).
	TaskDescriptionPath = TaskRoot + "/description"
	// TaskMetadataPath is the admitted task metadata (deterministic JSON object).
	TaskMetadataPath = TaskRoot + "/metadata.json"
	// TaskSpecPath is the admitted task specification JSON object.
	TaskSpecPath = TaskRoot + "/spec.json"
	// SecretRoot holds declared activation-time secret values.
	SecretRoot = "/kodema/secrets"
	// ReservedCredentialPath is the sole mutable mounted credential file. A
	// stored secret declaration can never shadow it.
	ReservedCredentialPath = SecretRoot + "/" + id.SecretTokenName
	// WorkspaceRoot is the writable workspace. Preparation never writes here.
	WorkspaceRoot = "/workspace"
)

// ValidatePath checks one materialized file path against the confined root
// rules. It refuses relative paths, empty paths, control bytes, trailing
// separators, empty or dot segments, any path outside the /kodema roots and the
// reserved credential mount.
func ValidatePath(path string) error {
	if path == "" {
		return fault.New(fault.InvalidInput, "materialized path is empty")
	}
	if !strings.HasPrefix(path, "/") {
		return fault.New(fault.InvalidInput, "materialized path must be absolute")
	}
	for index := 0; index < len(path); index++ {
		if path[index] < 0x20 || path[index] == 0x7f {
			return fault.New(fault.InvalidInput, "materialized path contains a control character")
		}
	}
	if strings.HasSuffix(path, "/") {
		return fault.New(fault.InvalidInput, "materialized path has a trailing separator")
	}
	segments := strings.Split(path, "/")
	for _, segment := range segments[1:] {
		if segment == "" || segment == "." || segment == ".." {
			return fault.New(fault.InvalidInput, "materialized path has an empty or relative segment")
		}
	}
	if path == ReservedCredentialPath {
		return fault.New(fault.InvalidInput, "materialized path is the reserved credential mount")
	}
	if path == TaskPath {
		return nil
	}
	for _, root := range []string{InputRoot, OutputRoot, TaskRoot, SecretRoot} {
		if strings.HasPrefix(path, root+"/") {
			return nil
		}
	}
	return fault.New(fault.InvalidInput, "materialized path is outside the /kodema roots")
}

// IsReservedPath reports whether the path is the reserved execution-credential
// mount. Stored secret declarations cannot use it.
func IsReservedPath(path string) bool {
	return path == ReservedCredentialPath
}

// maxTaskFileNameBytes bounds one filename under /kodema/task/ so that the
// base64url-encoded Kubernetes ConfigMap key for "/kodema/task/<name>" fits
// within Kubernetes's 253-character key limit (len("/kodema/task/") == 13;
// floor(253 * 3 / 4) - 13 == 176).
const maxTaskFileNameBytes = 176

// TaskFilePath returns the confined path for one admitted task specification
// file under /kodema/task/. Name must be a single safe filename without path
// separators, traversal, kubelet reserved ".." prefixes, or control characters.
func TaskFilePath(name string) (string, error) {
	if name == "" || name == "." || name == ".." || strings.HasPrefix(name, "..") || len(name) > maxTaskFileNameBytes {
		return "", fault.New(fault.InvalidInput, "task file name is invalid")
	}
	for index := 0; index < len(name); index++ {
		character := name[index]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '.', character == '_', character == '-':
		default:
			return "", fault.New(fault.InvalidInput, "task file name is invalid")
		}
	}
	path := TaskRoot + "/" + name
	if err := ValidatePath(path); err != nil {
		return "", err
	}
	return path, nil
}

// InputPath returns the confined path for one admitted input key.
func InputPath(key id.InputKey) (string, error) {
	if _, err := id.ParseInputKey(string(key)); err != nil {
		return "", fault.Wrap(fault.InvalidInput, "input key is invalid", err)
	}
	return InputRoot + "/" + string(key), nil
}

// OutputPath returns the confined path for one earlier succeeded output.
func OutputPath(action id.ActionName, key id.OutputKey) (string, error) {
	if _, err := id.ParseActionName(string(action)); err != nil {
		return "", fault.Wrap(fault.InvalidInput, "output action name is invalid", err)
	}
	if _, err := id.ParseOutputKey(string(key)); err != nil {
		return "", fault.Wrap(fault.InvalidInput, "output key is invalid", err)
	}
	return OutputRoot + "/" + string(action) + "/" + string(key), nil
}

// SecretPath returns the confined mount path for one declared secret name. The
// reserved credential name is refused.
func SecretPath(name id.SecretName) (string, error) {
	if _, err := id.ParseSecretName(string(name)); err != nil {
		return "", fault.Wrap(fault.InvalidInput, "secret name is invalid", err)
	}
	return SecretRoot + "/" + string(name), nil
}

// File is one immutable prepared file. Mode must be read-only: preparation
// never materializes a writable file outside /workspace.
type File struct {
	Path string
	Data []byte
	Mode os.FileMode
}

// Validate checks one prepared file path and its regular, read-only mode.
// A symlink, device, socket, pipe or directory mode is refused: preparation
// materializes ordinary immutable files, never an indirection that could
// escape the confined roots or turn into a writable device.
func (file File) Validate() error {
	if err := ValidatePath(file.Path); err != nil {
		return err
	}
	if file.Mode == 0 {
		return fault.New(fault.InvalidInput, "materialized file has no explicit mode")
	}
	if !file.Mode.IsRegular() {
		return fault.New(fault.InvalidInput, "materialized file mode is not a regular file")
	}
	if file.Mode&0o222 != 0 {
		return fault.New(fault.InvalidInput, "materialized file mode is writable")
	}
	return nil
}

// Materialization is the complete all-or-nothing preparation input for one
// workload. A workspace identity is required; file paths and secret names are
// unique; the task file exists only when a task snapshot is supplied.
type Materialization struct {
	WorkspaceID id.WorkspaceID
	Files       []File
	Secrets     []engine.SecretValue
	Credential  string
	Task        []byte
}

// Validate checks the whole preparation input. Any failure prevents start.
//
// Besides unique confined read-only regular files and unique valid declared
// secret names, it refuses a file path that is a directory prefix of another
// file, a file path that collides with a declared secret mount, and a missing
// execution credential. A partial or ambiguous snapshot is never authorized.
func (materialization Materialization) Validate() error {
	if _, err := id.ParseWorkspaceID(string(materialization.WorkspaceID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "materialization workspace id is invalid", err)
	}
	if materialization.Credential == "" {
		return fault.New(fault.InvalidInput, "materialization has no execution credential")
	}
	paths := make(map[string]bool, len(materialization.Files))
	for _, file := range materialization.Files {
		if paths[file.Path] {
			return fault.New(fault.InvalidInput, "materialization declares a duplicate file path")
		}
		for existing := range paths {
			if pathsCollide(existing, file.Path) {
				return fault.New(fault.InvalidInput, "materialization declares colliding file paths")
			}
		}
		paths[file.Path] = true
		if err := file.Validate(); err != nil {
			return err
		}
		if (file.Path == TaskPath || strings.HasPrefix(file.Path, TaskRoot+"/")) && materialization.Task == nil {
			return fault.New(fault.InvalidInput, "materialization declares a task file without a task snapshot")
		}
	}
	names := make(map[id.SecretName]bool, len(materialization.Secrets))
	secretPaths := make(map[string]bool, len(materialization.Secrets))
	for _, secret := range materialization.Secrets {
		if _, err := id.ParseSecretName(string(secret.Name)); err != nil {
			return fault.Wrap(fault.InvalidInput, "materialization declares an invalid secret name", err)
		}
		if names[secret.Name] {
			return fault.New(fault.InvalidInput, "materialization declares a duplicate secret name")
		}
		names[secret.Name] = true
		secretPath, err := SecretPath(secret.Name)
		if err != nil {
			return err
		}
		secretPaths[secretPath] = true
		for existing := range paths {
			if pathsCollide(existing, secretPath) {
				return fault.New(fault.InvalidInput, "materialization declares a file path that collides with a secret mount")
			}
		}
	}
	for existing := range secretPaths {
		for candidate := range secretPaths {
			if existing != candidate && pathsCollide(existing, candidate) {
				return fault.New(fault.InvalidInput, "materialization declares colliding secret mount paths")
			}
		}
	}
	return nil
}

// pathsCollide reports whether one materialized path equals another or is a
// directory prefix of it. "/kodema/inputs/a" collides with "/kodema/inputs/a/b"
// because the first would have to be both a file and a directory.
func pathsCollide(left, right string) bool {
	if left == right {
		return true
	}
	return strings.HasPrefix(left, right+"/") || strings.HasPrefix(right, left+"/")
}

// Preparation is the evidence of one materialization attempt. Complete reports
// that every file and declared secret was written; WorkspaceID is the attached
// retained workspace.
type Preparation struct {
	Complete    bool
	WorkspaceID id.WorkspaceID
}

// StartAllowed reports whether the preparation is complete enough to authorize
// a start. Partial preparation never does.
func (preparation Preparation) StartAllowed() bool {
	return preparation.Complete && preparation.WorkspaceID != ""
}

// Materializer performs one all-or-nothing preparation. A failure leaves no
// usable workload and prevents start.
type Materializer interface {
	Materialize(context.Context, Materialization) (Preparation, error)
}
