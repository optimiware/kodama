// Markdown-knowledge implementation API.
//
// This file is the test-side declaration of the behavior of
// internal/userland/skill. The base declarations in types.go (MaxBodyBytes,
// Name, ParseName, Skill, Skill.Validate, Loader) are not redefined here.
//
// Package declarations:
//
//	type Source interface {
//	    List(ctx context.Context) ([]Name, error)
//	    Read(ctx context.Context, name Name) (Skill, error)
//	}
//	func NewDirSource(root string) (*DirSource, error)
//	type DirSource struct{ ... } // implements Source
//	func NewLoader(source Source) (*MarkdownLoader, error)
//	type MarkdownLoader struct{ ... } // implements Loader
//	func ContainsSecret(body []byte) bool
//
// NewDirSource resolves root to an absolute path and Lstat's every path
// component: a symlinked component, a missing root or a non-directory root
// refuses. The source is confined to that directory. Read maps a name to
// <root>/<name>.md only after ParseName, so traversal, absolute names, nested
// paths, a dotted name and an extension refuse before any filesystem access.
// The leaf must be a regular file; a symlink, directory or device refuses. A
// file larger than MaxBodyBytes refuses; at most MaxBodyBytes+1 bytes are read.
// Read returns Skill{Name: name, Path: <name>.md}. List reports the names of
// regular .md files whose stem parses as a Name, in any order, and never
// follows a symlink or descends into a directory.
//
// NewLoader refuses a nil source. MarkdownLoader.Load validates the requested
// name before calling the source, refuses a source result whose Name differs
// from the request, applies Skill.Validate, and refuses a body for which
// ContainsSecret is true. MarkdownLoader.List returns the source names sorted
// ascending and de-duplicated, independent of enumeration order; an invalid
// name from the source refuses.
//
// ContainsSecret reports true when body carries:
//   - a PEM private key header, case-insensitively: "-----BEGIN " ...
//     " PRIVATE KEY-----";
//   - an authorization bearer header: case-insensitive "authorization:",
//     optional spaces, case-insensitive "bearer " and a non-empty token;
//   - an AWS-style access key id, case-insensitively: "AKIA" followed by 16
//     alphanumerics;
//   - a case-insensitive assignment of "password", "passwd", "secret",
//     "token", "api_key", "access_key", "private_key" or an
//     "access_key"/"secret_access_key"-style compound, followed by '=' or ':'
//     and a non-empty value. The label may be the suffix of an identifier
//     ("GITHUB_TOKEN", "db_password"), may sit behind a quoted JSON/YAML key
//     ("api_key": "x") and may be exported ("export NAME=value"); an empty
//     value or an identifier that only contains a credential-ish word is
//     benign.
//
// It is a refusal gate for supplied knowledge, never a redactor.
package skill_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/userland/skill"
)

// TestContainsSecret pins the secret gate for supplied knowledge.
func TestContainsSecret(t *testing.T) {
	secrets := []string{
		"-----BEGIN RSA PRIVATE KEY-----\nMIIEow\n-----END RSA PRIVATE KEY-----",
		"-----BEGIN PRIVATE KEY-----\nMIIEow",
		"Authorization: Bearer abc123",
		"authorization:   bearer abc123",
		"aws_access_key_id = AKIAIOSFODNN7EXAMPLE",
		"password=hunter2",
		"password: hunter2",
		"secret=abc123",
		"token: abc123",
		"API_KEY=abc123",
		"api_key: abc123",
		"Api-Key = abc123",
		"ACCESS_KEY=abc123",
		"Private_Key=abc123",
		// A credential label carried as the suffix of an identifier still
		// labels a credential: '_' is a word character, so the label has no
		// word boundary of its own.
		"GITHUB_TOKEN=ghp_abc",
		"API_TOKEN=abc",
		"AWS_SECRET_ACCESS_KEY=abc",
		"aws_secret_access_key = abc",
		"db_password=abc",
		"client_secret: abc",
		"MY_API_KEY = \"abc\"",
		"x-api-key: abc",
		// Quoted JSON/YAML forms and shell exports are the same assignment.
		`{"api_key": "x"}`,
		`"api_key": "x"`,
		`"token":"abc"`,
		`api_key: "abc`,
		"export GITHUB_TOKEN=ghp_abc",
		"export AWS_SECRET_ACCESS_KEY=abc",
		"Authorization: BEARER abc123",
		"authorization: BeArEr abc123",
		"-----BEGIN private key-----\nMIIEow",
		"-----BEGIN rsa private key-----\nMIIEow",
		"AKIAiosfodnn7example",
	}
	for _, body := range secrets {
		if !skill.ContainsSecret([]byte(body)) {
			t.Errorf("ContainsSecret(%q) = false; want true", body)
		}
	}

	benign := []string{
		"",
		"# Code review\n\nRead the diff and list risks.\n",
		"Rotate the password every 90 days.",
		"Bearer tokens are explained in the security guide.",
		"-----BEGIN PUBLIC KEY-----\nMIIB\n-----END PUBLIC KEY-----",
		"AKIA",
		"password=",
		"token: ",
		"API_KEY=",
		"api_key: ",
		"The API_KEY setting is documented.",
		// Empty assignments stay benign in quoted form too, and an identifier
		// that merely contains a credential-ish word is not an assignment.
		`{"api_key": ""}`,
		`api_key: ""`,
		"password: ''",
		"export PATH=/usr/bin",
		"max_tokens: 4096",
		"Tokens: none",
		"the token was abc",
	}
	for _, body := range benign {
		if skill.ContainsSecret([]byte(body)) {
			t.Errorf("ContainsSecret(%q) = true; want false", body)
		}
	}

	if skill.MaxBodyBytes != 1<<20 {
		t.Fatalf("MaxBodyBytes = %d; want 1 MiB", skill.MaxBodyBytes)
	}
}
