package acceptance_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestGeneratedAppIndependent: the generated module builds and tests outside the
// factory checkout, contains no factory or Platform imports or embedded
// credentials, and records reproducible generation inputs. No services.
func TestGeneratedAppIndependent(t *testing.T) {
	app := generate(t)

	t.Run("builds, vets and passes its service-free tests on its own", func(t *testing.T) {
		mustRun(t, app, nil, "go", "build", "./...")
		mustRun(t, app, nil, "go", "vet", "./...")
		// Real-PostgreSQL tests skip here (no admin URL); everything else must pass.
		results := goTest(t, app, nil, "", "./internal/...", "./cmd/...", "./migrations/...")
		if len(results) < 30 {
			t.Fatalf("only %d tests ran", len(results))
		}
		for name, r := range results {
			if r == "fail" {
				t.Errorf("%s failed", name)
			}
		}
	})

	t.Run("imports nothing from Kodama or Platform", func(t *testing.T) {
		deps := mustRun(t, app, nil, "go", "list", "-deps", "-test", "./...")
		for _, line := range strings.Split(deps, "\n") {
			if strings.HasPrefix(line, "github.com/optimiware/kodama/") || strings.Contains(line, "optimi.io/platform") || strings.Contains(line, "optimiweb") {
				t.Errorf("forbidden dependency %s", line)
			}
		}
		mod := readFile(t, filepath.Join(app, "go.mod"))
		if !strings.HasPrefix(mod, "module "+modulePath+"\n") {
			t.Errorf("module path not substituted:\n%s", mod)
		}
		for _, line := range strings.Split(mod, "\n") {
			l := strings.TrimSpace(line)
			if strings.HasPrefix(l, "github.com/optimiware/kodama") || strings.Contains(l, "replace") {
				t.Errorf("go.mod must not point at the checkout: %q", l)
			}
		}
		err := filepath.WalkDir(app, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || strings.HasPrefix(p, filepath.Join(app, ".git")+string(filepath.Separator)) || !strings.HasSuffix(p, ".go") {
				return err
			}
			if strings.Contains(readFile(t, p), "example.invalid/inventory") {
				t.Errorf("%s still names the template module", p)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("contains no embedded credentials", func(t *testing.T) {
		secret := regexp.MustCompile(`(?i)(postgres(ql)?://[^\s:@/]+:[^\s@/]+@|-----BEGIN [A-Z ]*PRIVATE KEY-----|AKIA[0-9A-Z]{16}|ghp_[A-Za-z0-9]{20,}|xox[bp]-[A-Za-z0-9-]{10,})`)
		_ = filepath.WalkDir(app, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(app, p)
			if strings.HasPrefix(rel, ".git"+string(filepath.Separator)) || strings.HasSuffix(p, "_test.go") {
				return nil // tests carry deliberate negative samples
			}
			if m := secret.FindString(readFile(t, p)); m != "" {
				t.Errorf("%s contains what looks like a credential: %q", rel, m)
			}
			return nil
		})
	})

	t.Run("records reproducible generation inputs", func(t *testing.T) {
		var rec struct {
			Template, TemplateVersion, TemplateLockSha256, Module, TemplateModule, TreeSha256 string
		}
		if err := json.Unmarshal([]byte(readFile(t, filepath.Join(app, "GENERATED_FROM.json"))), &rec); err != nil {
			t.Fatal(err)
		}
		if rec.Template != "go-postgres" || rec.TemplateVersion == "" || rec.Module != modulePath || len(rec.TreeSha256) != 64 || len(rec.TemplateLockSha256) != 64 {
			t.Fatalf("%+v", rec)
		}
		again := generate(t)
		var rec2 struct{ TreeSha256 string }
		_ = json.Unmarshal([]byte(readFile(t, filepath.Join(again, "GENERATED_FROM.json"))), &rec2)
		if rec2.TreeSha256 != rec.TreeSha256 {
			t.Errorf("generation is not reproducible: %s vs %s", rec.TreeSha256, rec2.TreeSha256)
		}
		h1 := strings.TrimSpace(mustRun(t, app, nil, "git", "rev-parse", "HEAD"))
		h2 := strings.TrimSpace(mustRun(t, again, nil, "git", "rev-parse", "HEAD"))
		if h1 != h2 || len(h1) != 40 {
			t.Errorf("the generated application commit must be reproducible: %s vs %s", h1, h2)
		}
		if st := mustRun(t, app, nil, "git", "status", "--porcelain"); strings.TrimSpace(st) != "" {
			t.Errorf("the generated commit must contain the whole tree:\n%s", st)
		}
		// The pins recorded by the lock agree with the files that enforce them.
		lock := readFile(t, filepath.Join(templateDir(t), "template.lock.json"))
		for _, want := range []string{"v5.11.0", "v3.21.0", "1.27.2"} {
			if !strings.Contains(lock, want) {
				t.Errorf("template.lock.json lacks %s", want)
			}
		}
		if !strings.Contains(readFile(t, filepath.Join(app, "go.mod")), "github.com/jackc/pgx/v5 v5.11.0") || !strings.Contains(readFile(t, filepath.Join(app, "go.mod")), "go 1.27.2") {
			t.Error("go.mod does not carry the locked versions")
		}
		koBase := regexp.MustCompile(`sha256:[0-9a-f]{64}`).FindString(readFile(t, filepath.Join(app, ".ko.yaml")))
		if !strings.Contains(lock, koBase) || koBase == "" {
			t.Error(".ko.yaml base image digest differs from the lock")
		}
		for _, line := range strings.Split(readFile(t, filepath.Join(app, "deploy/tools/lock.env")), "\n") {
			if k, v, ok := strings.Cut(line, "="); ok && strings.HasPrefix(k, "KO_SHA256_") || ok && strings.HasPrefix(k, "KUBECTL_SHA256_") {
				if !strings.Contains(lock, v) {
					t.Errorf("%s differs from template.lock.json", k)
				}
			}
		}
	})

	t.Run("preserves executable scripts", func(t *testing.T) {
		for _, s := range []string{"quality.sh", "build.sh", "deploy-preview.sh", "deploy-stage.sh", "e2e.sh", "render-pipelines.sh"} {
			st, err := os.Stat(filepath.Join(app, "ci", s))
			if err != nil || st.Mode()&0o111 == 0 {
				t.Errorf("ci/%s is not executable: %v", s, err)
			}
		}
	})

	t.Run("refuses unsafe module paths and non-empty destinations", func(t *testing.T) {
		gen := filepath.Join(templateDir(t), "generate.sh")
		for _, bad := range []string{"", "no-dot/inventory", "a.b/c d", "a.b/c;rm", "a.b/../c", "a.b/c|x", "a.b/", "A.b/x&y", "a.b/c\"d"} {
			dest := filepath.Join(t.TempDir(), "x")
			if out, err := run(t, "", nil, "sh", gen, "--module", bad, "--dest", dest); err == nil {
				t.Errorf("module %q must be refused:\n%s", bad, out)
			}
			if _, err := os.Stat(filepath.Join(dest, "go.mod")); err == nil {
				t.Errorf("module %q produced output", bad)
			}
		}
		if out, err := run(t, "", nil, "sh", gen, "--module", modulePath, "--dest", app); err == nil {
			t.Errorf("a non-empty destination must be refused:\n%s", out)
		}
	})
}
