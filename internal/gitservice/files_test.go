package gitservice

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

func (e *testEnv) commitFiles(dir, message string, files map[string]string, remove ...string) string {
	e.t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			e.t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			e.t.Fatal(err)
		}
		e.mustPlain(dir, "add", name)
	}
	for _, name := range remove {
		e.mustPlain(dir, "rm", "-q", name)
	}
	e.mustPlain(dir, "commit", "-q", "-m", message)
	return e.mustPlain(dir, "rev-parse", "HEAD")
}

// Test_unit_gitservice_read_files: pipeline definitions are read at the exact
// commit named, a directory request expands to the direct .yaml and .yml files,
// a missing path is absent, a missing commit is commit_not_found, anything
// outside .kodama/ or beyond the bounds is refused, and a pushed replacement
// object changes nothing.
func Test_unit_gitservice_read_files(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.initRepo("repo_1", "main")
	w := e.workdir()
	first := e.commitFiles(w, "one", map[string]string{
		".kodama/build.yaml": "name: build\n", ".kodama/deploy.yml": "name: deploy\n", ".kodama/notes.txt": "x",
		".kodama/nested/inner.yaml": "name: inner\n", "src/main.go": "package main\n",
	})
	second := e.commitFiles(w, "two", map[string]string{".kodama/build.yaml": "name: build\n# second\n"}, ".kodama/deploy.yml")
	e.seed("repo_1", w, "main:refs/heads/main")

	const path = "/repositories/repo_1/files"
	read := func(commit string, paths ...string) map[string]gitv1.DefinitionFile {
		t.Helper()
		resp := rpcOK[gitv1.ReadFilesResponse](e, path, gitv1.ReadFilesRequest{RepositoryID: "repo_1", Commit: commit, Paths: paths})
		out := map[string]gitv1.DefinitionFile{}
		for _, f := range resp.Files {
			out[f.Path] = f
		}
		if len(out) != len(resp.Files) {
			t.Fatalf("duplicate files: %+v", resp.Files)
		}
		return out
	}

	t.Run("directory request reads the exact commit", func(t *testing.T) {
		got := read(first, ".kodama/")
		if len(got) != 2 || got[".kodama/build.yaml"].Content != "name: build\n" || got[".kodama/deploy.yml"].Content != "name: deploy\n" {
			t.Fatalf("at the first commit: %+v", got)
		}
		if got[".kodama/build.yaml"].SHA256 != sha256Hex([]byte("name: build\n")) {
			t.Fatalf("sha256: %+v", got[".kodama/build.yaml"])
		}
		later := read(second, ".kodama/")
		if len(later) != 1 || later[".kodama/build.yaml"].Content != "name: build\n# second\n" {
			t.Fatalf("at the second commit (deploy removed, build changed): %+v", later)
		}
	})
	t.Run("named files, a missing one is absent", func(t *testing.T) {
		got := read(second, ".kodama/build.yaml", ".kodama/deploy.yml", ".kodama/nested/inner.yaml", ".kodama/build.yaml")
		if len(got) != 2 || got[".kodama/nested/inner.yaml"].Content != "name: inner\n" {
			t.Fatalf("%+v", got)
		}
		if got := read(second, ".kodama/none.yaml"); len(got) != 0 {
			t.Fatalf("a missing file is absent, got %+v", got)
		}
	})
	t.Run("a commit without the directory has no files", func(t *testing.T) {
		e.mustPlain(w, "checkout", "-q", "--orphan", "bare")
		e.mustPlain(w, "rm", "-rfq", ".")
		bare := e.commitFiles(w, "bare", map[string]string{"README": "r\n"})
		e.seed("repo_1", w, "bare:refs/heads/bare")
		if got := read(bare, ".kodama/"); len(got) != 0 {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("a missing or non-commit object is commit_not_found", func(t *testing.T) {
		tree := e.mustPlain(w, "rev-parse", first+"^{tree}")
		for _, c := range []string{strings.Repeat("a", 40), tree} {
			rpcError(e, path, gitv1.ReadFilesRequest{RepositoryID: "repo_1", Commit: c, Paths: []string{".kodama/"}}, http.StatusNotFound, gitv1.CodeCommitNotFound)
		}
		rpcError(e, "/repositories/repo_x/files", gitv1.ReadFilesRequest{RepositoryID: "repo_x", Commit: first, Paths: []string{".kodama/"}}, http.StatusNotFound, domain.CodeNotFound)
	})
	t.Run("only definitions under .kodama are requested", func(t *testing.T) {
		for _, p := range []string{"src/main.go", ".kodama/notes.txt", ".kodama/../src/main.go", "/etc/passwd", ".kodama/a:b.yaml", ".kodama/x\n.yaml",
			".kodama//x.yaml", ".kodama/.yaml", ".kodama/", "", ".kodama", ".kodama/sub/", "kodama/x.yaml", "../.kodama/x.yaml", ".git/config.yaml"} {
			if p == ".kodama/" {
				continue // the one directory request
			}
			rpcError(e, path, gitv1.ReadFilesRequest{RepositoryID: "repo_1", Commit: first, Paths: []string{p}}, http.StatusBadRequest, domain.CodeInvalidRequest)
		}
		rpcError(e, path, gitv1.ReadFilesRequest{RepositoryID: "repo_1", Commit: "main", Paths: []string{".kodama/"}}, http.StatusBadRequest, domain.CodeInvalidRequest)
		rpcError(e, path, gitv1.ReadFilesRequest{RepositoryID: "repo_1", Commit: strings.Repeat("0", 40), Paths: []string{".kodama/"}}, http.StatusBadRequest, domain.CodeInvalidRequest)
		rpcError(e, path, gitv1.ReadFilesRequest{RepositoryID: "repo_1", Commit: first}, http.StatusBadRequest, domain.CodeInvalidRequest)
		rpcError(e, "/repositories/repo_2/files", gitv1.ReadFilesRequest{RepositoryID: "repo_1", Commit: first, Paths: []string{".kodama/"}}, http.StatusBadRequest, domain.CodeInvalidRequest)
		if status, _ := e.rpcWith(http.MethodPost, path, gitv1.ReadFilesRequest{RepositoryID: "repo_1", Commit: first, Paths: []string{".kodama/"}}, ""); status != http.StatusUnauthorized {
			t.Errorf("an unauthenticated call: %d", status)
		}
	})
	t.Run("a symbolic link is read as a link, never followed", func(t *testing.T) {
		e.mustPlain(w, "checkout", "-q", "main")
		if err := os.MkdirAll(filepath.Join(w, ".kodama"), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("/etc/passwd", filepath.Join(w, ".kodama", "link.yaml")); err != nil {
			t.Skip("symlinks are unavailable")
		}
		e.mustPlain(w, "add", ".kodama/link.yaml")
		e.mustPlain(w, "commit", "-q", "-m", "link")
		link := e.mustPlain(w, "rev-parse", "HEAD")
		e.seed("repo_1", w, "main:refs/heads/linked")
		if got := read(link, ".kodama/link.yaml"); got[".kodama/link.yaml"].Content != "/etc/passwd" {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("bounds refuse the whole request", func(t *testing.T) {
		big := map[string]string{".kodama/huge.yaml": strings.Repeat("a", gitv1.MaxReadFileBytes+1)}
		c := e.commitFiles(w, "big", big)
		e.seed("repo_1", w, "main:refs/heads/big")
		rpcError(e, path, gitv1.ReadFilesRequest{RepositoryID: "repo_1", Commit: c, Paths: []string{".kodama/"}}, http.StatusBadRequest, domain.CodeInvalidRequest)
		rpcError(e, path, gitv1.ReadFilesRequest{RepositoryID: "repo_1", Commit: c, Paths: []string{".kodama/huge.yaml"}}, http.StatusBadRequest, domain.CodeInvalidRequest)

		many := map[string]string{}
		for i := 0; i < gitv1.MaxReadFilePaths+1; i++ {
			many[".kodama/p"+strings.Repeat("x", i%5)+string(rune('a'+i/5))+string(rune('a'+i%5))+".yaml"] = "n: 1\n"
		}
		w2 := e.workdir()
		c2 := e.commitFiles(w2, "many", many)
		e.seed("repo_1", w2, "main:refs/heads/many")
		rpcError(e, path, gitv1.ReadFilesRequest{RepositoryID: "repo_1", Commit: c2, Paths: []string{".kodama/"}}, http.StatusBadRequest, domain.CodeInvalidRequest)
		paths := make([]string, 0, gitv1.MaxReadFilePaths+1)
		for p := range many {
			paths = append(paths, p)
		}
		rpcError(e, path, gitv1.ReadFilesRequest{RepositoryID: "repo_1", Commit: c2, Paths: paths}, http.StatusBadRequest, domain.CodeInvalidRequest)
	})
	t.Run("a file that is not UTF-8 text refuses the request", func(t *testing.T) {
		w3 := e.workdir()
		c := e.commitFiles(w3, "binary", map[string]string{".kodama/bin.yaml": "name: \xff\xfe\n"})
		e.seed("repo_1", w3, "main:refs/heads/binary")
		rpcError(e, path, gitv1.ReadFilesRequest{RepositoryID: "repo_1", Commit: c, Paths: []string{".kodama/"}}, http.StatusBadRequest, domain.CodeInvalidRequest)
	})
	t.Run("a pushed replace ref does not change the file", func(t *testing.T) {
		e.mustPlain(w, "checkout", "-q", "--orphan", "evil")
		e.mustPlain(w, "rm", "-rfq", ".")
		evil := e.commitFiles(w, "evil", map[string]string{".kodama/build.yaml": "name: evil\n"})
		e.seed("repo_1", w, "evil:refs/heads/evil", evil+":refs/replace/"+second)
		got := read(second, ".kodama/build.yaml")
		if got[".kodama/build.yaml"].Content != "name: build\n# second\n" {
			t.Fatalf("a replace ref changed the file: %+v", got)
		}
	})
}
