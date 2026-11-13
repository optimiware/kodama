// Package cli declares kodactl configuration, sessions, output formats and the
// Git credential-helper contract.
//
// The declarations take their filesystem, client and output collaborators as
// injected ports (Ports, FileSystem) and never read a file, call a server or
// print a credential themselves.
package cli

import (
	"context"
	"io"
	"io/fs"
	"time"

	"github.com/optimiware/kodama/client"
)

// OutputFormat is one kodactl output format.
type OutputFormat string

const (
	// OutputTable renders human-readable tables.
	OutputTable OutputFormat = "table"
	// OutputJSON renders JSON.
	OutputJSON OutputFormat = "json"
)

// outputFormats is the declaration order.
var outputFormats = []OutputFormat{OutputTable, OutputJSON}

// Valid reports whether the format is one of the declared formats.
func (format OutputFormat) Valid() bool {
	for _, candidate := range outputFormats {
		if candidate == format {
			return true
		}
	}
	return false
}

// Config is the kodactl configuration input.
type Config struct {
	ServerURL      string       `json:"server_url"`
	CredentialFile string       `json:"credential_file,omitempty"`
	CAFile         string       `json:"ca_file,omitempty"`
	Output         OutputFormat `json:"output,omitempty"`
	TrustedOrigins []string     `json:"trusted_origins,omitempty"`
	Project        string       `json:"project,omitempty"`
}

// Exit codes returned by Run. Run never calls os.Exit.
const (
	// ExitSuccess reports a completed command.
	ExitSuccess = 0
	// ExitFailure reports a client, credential, persistence or file failure.
	ExitFailure = 1
	// ExitUsage reports invalid syntax, configuration or invocation.
	ExitUsage = 2
)

// DirEntry is one directory entry read through the injected filesystem.
type DirEntry struct {
	Name  string
	IsDir bool
}

// FileSystem is the injected kodactl filesystem boundary. It is a small
// consumer-owned port so command logic never opens a real file in unit tests.
//
// ReplaceFile is the durable-replace operation: it creates missing parent
// directories with mode 0700 (never changing an existing directory's mode),
// writes data to an exclusively created 0600 temporary file in the same
// directory, fsyncs it, sets the final mode perm, atomically renames it over
// name and fsyncs the directory. Any failure removes the temporary file and
// leaves the previous file untouched. A symlink at name is replaced as a link,
// not followed; a directory at name is refused.
//
// Remove deletes one file so logout can discard the session through the same
// injected boundary. A directory is refused; a symlink is removed as a link,
// not followed; a missing file is returned as an error the caller classifies
// with errors.Is(err, fs.ErrNotExist). ReadFile, WriteFile and ReadDir keep
// their existing behavior.
type FileSystem interface {
	ReadFile(name string) ([]byte, error)
	WriteFile(name string, data []byte, perm fs.FileMode) error
	ReplaceFile(name string, data []byte, perm fs.FileMode) error
	Remove(name string) error
	ReadDir(name string) ([]DirEntry, error)
}

// SessionStore is the durable human-session boundary.
type SessionStore interface {
	Load(ctx context.Context) (client.Credential, error)
	Save(ctx context.Context, credential client.Credential) error
}

// FileSessionStore persists one human session as JSON in an injected file. It
// reads the file again on every Load so a rotated session takes effect.
type FileSessionStore struct {
	Path  string
	Files FileSystem
	Now   func() time.Time
}

// Ports are the injected kodactl collaborators. A command only uses the ports
// it needs; a missing required port is a failure, never a panic.
type Ports struct {
	Client     client.Transport
	Files      FileSystem
	Session    SessionStore
	Credential client.CredentialSource
	In         io.Reader
	Out        io.Writer
	Err        io.Writer
	Now        func() time.Time
	Sleep      func(time.Duration)
}

// Origin is one parsed trusted Git origin.
type Origin struct {
	Scheme string `json:"scheme"`
	Host   string `json:"host"`
	Path   string `json:"path,omitempty"`
}

// Session is one kodactl session input. A human session and a mounted execution
// token are separate sources; a service credential is not a helper session.
type Session struct {
	Source    client.CredentialKind `json:"source"`
	TokenPath string                `json:"token_path"`
	ExpiresAt time.Time             `json:"expires_at"`
}

// HelperRequest is one Git credential-helper request.
type HelperRequest struct {
	Protocol string `json:"protocol"`
	URL      string `json:"url"`
}

// HelperResponse is one Git credential-helper response. String never renders the
// password.
type HelperResponse struct {
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}
