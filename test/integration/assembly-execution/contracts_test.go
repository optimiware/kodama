package assembly_execution_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// contractSuite is one port's shared Contract suite run against its real
// adapter: the PostgreSQL (and S3, Git, JetStream) twin of the memory adapter
// the unit layer runs.
type contractSuite struct {
	task string // the task that owns the suite
	pkg  string
	run  string // the exact tests, which run each suite with the real adapter
}

var contractSuites = []contractSuite{
	{"T-088", "./internal/events/eventstest", "^(TestPostgresEventsContract|TestJetStreamBrokerContract)$"},
	{"T-088", "./internal/work/worktest", "^TestPostgresWorkContract$"},
	{"T-088", "./internal/workspace/workspacetest", "^(TestPostgresWorkspaceContract|TestHTTPSGitContract)$"},
	{"T-088", "./internal/artifacts/artifactstest", "^(TestPostgresArtifactsContract|TestS3ArtifactObjectsContract)$"},
	{"T-088", "./internal/runtimeauth/rtest", "^TestPostgresRuntimeauthContract$"},
	{"T-088", "./internal/identity/idtest", "^TestPostgresIdentityContract$"},
	{"T-088", "./internal/secrets/secretstest", "^TestPostgresSecretsContract$"},
	{"T-080", "./internal/execution/postgres", "^Test_store_contract$"},
	{"T-081", "./internal/results/postgres", "^TestPostgresContract$"},
	{"T-081", "./internal/logs/postgres", "^TestPostgresContract$"},
}

// TestM1PostgresContractSuites runs every Contract suite of T-087/T-088/T-080/
// T-081 against the real adapters in this checkpoint's scope and records, per
// suite, the tests that passed. A suite that skips, fails or runs no test fails
// the checkpoint: the memory twin passing in the unit layer is not evidence of
// the real adapter. The summary is the record docs/acceptance/m1.md refers to.
// (T-087's memdb is the memory adapter itself; its twins are these suites.)
func TestM1PostgresContractSuites(t *testing.T) {
	root := repoRoot(t)
	record := os.Getenv("KODAMA_M1_CONTRACT_RECORD")
	var summary []string
	for _, s := range contractSuites {
		t.Run(strings.TrimPrefix(s.pkg, "./internal/")+"/"+s.task, func(t *testing.T) {
			cmd := exec.Command("go", "test", "-json", "-race", "-count=1", "-run", s.run, s.pkg)
			cmd.Dir = root
			var out bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &out
			done := make(chan error, 1)
			started := time.Now()
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			go func() { done <- cmd.Wait() }()
			var err error
			select {
			case err = <-done:
			case <-time.After(15 * time.Minute):
				_ = cmd.Process.Kill()
				t.Fatalf("%s timed out", s.pkg)
			}
			passed, failed, skipped, text := countTests(out.Bytes())
			// A suite may decline one case for an adapter that proves it elsewhere (the Git
			// contract's scratch-bound case); a suite that is skipped as a whole, or a run
			// that passed nothing, is no evidence.
			var skippedSuites []string
			for _, sk := range skipped {
				if !strings.Contains(sk, "/") {
					skippedSuites = append(skippedSuites, sk)
				}
			}
			if err != nil || len(failed) > 0 || len(skippedSuites) > 0 || len(passed) == 0 {
				t.Fatalf("%s %s: %v\nfailed %v skipped %v passed %d\n%s", s.pkg, s.run, err, failed, skipped, len(passed), tail(text, 40))
			}
			declined := ""
			if len(skipped) > 0 {
				declined = ", " + itoa(len(skipped)) + " declined by the adapter's own contract"
			}
			top := 0
			for _, p := range passed {
				if !strings.Contains(p, "/") {
					top++
				}
			}
			summary = append(summary, s.task+" "+s.pkg+": "+itoa(top)+" suites, "+itoa(len(passed)-top)+" subtests passed"+declined+" in "+time.Since(started).Round(time.Second).String())
		})
	}
	sort.Strings(summary)
	t.Logf("contract suites on the real adapters:\n  %s", strings.Join(summary, "\n  "))
	if record != "" {
		if err := os.WriteFile(record, []byte(strings.Join(summary, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func repoRoot(t testing.TB) string {
	t.Helper()
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(strings.TrimSpace(string(out)))
}

func countTests(out []byte) (passed, failed, skipped []string, text string) {
	var buf strings.Builder
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		var ev struct {
			Action string
			Test   string
			Output string
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			buf.WriteString(sc.Text() + "\n")
			continue
		}
		if ev.Action == "output" {
			buf.WriteString(ev.Output)
		}
		if ev.Test == "" {
			continue
		}
		switch ev.Action {
		case "pass":
			passed = append(passed, ev.Test)
		case "fail":
			failed = append(failed, ev.Test)
		case "skip":
			skipped = append(skipped, ev.Test)
		}
	}
	return passed, failed, skipped, buf.String()
}

func tail(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
