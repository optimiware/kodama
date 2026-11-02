package bootstrap

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
)

const testGitCredential = "gc_0123456789abcdefABCDEF.-_~+/="

func ptr[T any](v T) *T { return &v }

func Test_unit_git_files_are_private_atomic_and_exactly_the_delivery(t *testing.T) {
	ca := testPEM(t)
	d := runtimev3.GitDelivery{CredentialFile: ptr("/kodama/git/credential"), CAFile: ptr("/kodama/git/ca.pem")}
	dir := filepath.Join(t.TempDir(), "git")
	if err := WriteGit(dir, d, GitMaterial{Credential: []byte(testGitCredential), CA: ca}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"credential": testGitCredential + "\n", "ca.pem": string(ca)} {
		fi, err := os.Stat(filepath.Join(dir, name))
		b, _ := os.ReadFile(filepath.Join(dir, name))
		if err != nil || fi.Mode().Perm() != 0o600 || string(b) != want {
			t.Fatalf("%s: %v %v %q", name, fi, err, b)
		}
	}
	if di, _ := os.Stat(dir); di.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode %v", di.Mode())
	}
	// A redrive replaces the credential atomically and leaves no temporary file.
	if err := WriteGit(dir, d, GitMaterial{Credential: []byte("gc_another_credential_0123456789"), CA: ca}); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("%d entries", len(entries))
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "credential")); string(b) != "gc_another_credential_0123456789\n" {
		t.Fatalf("credential %q", b)
	}
	if s := fmt.Sprintf("%v %+v %#v", GitMaterial{Credential: []byte(testGitCredential)}, GitMaterial{Credential: []byte(testGitCredential)}, GitMaterial{Credential: []byte(testGitCredential)}); strings.Contains(s, testGitCredential) {
		t.Fatal("the credential is in the formatted output")
	}
}

func Test_unit_git_files_refuse_what_the_delivery_does_not_name(t *testing.T) {
	ca := testPEM(t)
	d := runtimev3.GitDelivery{CredentialFile: ptr("/kodama/git/credential"), CAFile: nil}
	for name, tc := range map[string]struct {
		d runtimev3.GitDelivery
		m GitMaterial
	}{
		"a credential for a delivery without one":     {runtimev3.GitDelivery{}, GitMaterial{Credential: []byte(testGitCredential)}},
		"no credential for a delivery that names one": {d, GitMaterial{}},
		"a CA for a delivery without one":             {d, GitMaterial{Credential: []byte(testGitCredential), CA: ca}},
		"a username:password line":                    {d, GitMaterial{Credential: []byte("kodama:" + testGitCredential)}},
		"a credential with whitespace":                {d, GitMaterial{Credential: []byte(testGitCredential + "\n")}},
		"a short credential":                          {d, GitMaterial{Credential: []byte("short")}},
		"a path outside the directory":                {runtimev3.GitDelivery{CredentialFile: ptr("/kodama/identity/credential")}, GitMaterial{Credential: []byte(testGitCredential)}},
		"a nested path":                               {runtimev3.GitDelivery{CredentialFile: ptr("/kodama/git/a/credential")}, GitMaterial{Credential: []byte(testGitCredential)}},
		"a traversal":                                 {runtimev3.GitDelivery{CredentialFile: ptr("/kodama/git/../x")}, GitMaterial{Credential: []byte(testGitCredential)}},
		"one file for both":                           {runtimev3.GitDelivery{CredentialFile: ptr("/kodama/git/x"), CAFile: ptr("/kodama/git/x")}, GitMaterial{Credential: []byte(testGitCredential), CA: ca}},
		"a CA that is not a certificate":              {runtimev3.GitDelivery{CredentialFile: ptr("/kodama/git/c"), CAFile: ptr("/kodama/git/ca")}, GitMaterial{Credential: []byte(testGitCredential), CA: []byte("not pem")}},
	} {
		dir := t.TempDir()
		err := WriteGit(dir, tc.d, tc.m)
		if !errors.Is(err, ErrGitMaterial) || strings.Contains(err.Error(), testGitCredential) {
			t.Fatalf("%s: %v", name, err)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Fatalf("%s: wrote %d files", name, len(entries))
		}
	}
}

func Test_unit_git_files_provisioning_and_preparation_and_removal(t *testing.T) {
	// A provisioning run without sources holds no static grant: only the directory.
	dir := filepath.Join(t.TempDir(), "git")
	if err := WriteGit(dir, runtimev3.GitDelivery{}, GitMaterial{}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("the directory for runtime-issued credentials is missing: %v", err)
	}
	// The preparation's read-only grant is delivered the same way.
	g := runtimev3.PreparationGit{GrantID: "gg_p", BaseURL: "https://git.example.invalid/git", CredentialFile: "/kodama/git/credential", Repositories: []string{"repo_01"}}
	if err := WriteGitPreparation(dir, g, GitMaterial{Credential: []byte(testGitCredential)}); err != nil {
		t.Fatal(err)
	}
	// The files the entrypoint wrote at runtime go with the rest when the attempt ends.
	if err := os.WriteFile(filepath.Join(dir, "integration-credential"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveGit(dir); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("%d files survived", len(entries))
	}
	if err := RemoveGit(filepath.Join(dir, "absent")); err != nil {
		t.Fatalf("removing nothing: %v", err)
	}
}
