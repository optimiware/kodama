//go:build component

// Hermetic tests for the application-database resolution in
// scripts/stage-reset. The fake kubectl captures the one-shot pod manifests;
// the database shell blocks they carry are then executed locally against a
// fake psql, so no cluster, network or database is touched.
package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// fakeStageResetPsql records the libpq environment and the SQL it was given.
// The ledger probe (a -c query) answers FAKE_PSQL_CHECK; every other call
// consumes the SQL on stdin into FAKE_PSQL_STDIN.
const fakeStageResetPsql = `#!/usr/bin/env bash
set -u
{
  printf 'call\t%s\n' "$*"
  printf 'PGDATABASE\t%s\n' "${PGDATABASE:-}"
  printf 'PGUSER\t%s\n' "${PGUSER:-}"
} >> "$FAKE_PSQL_LOG"
if [[ " $* " == *" -c "* ]]; then
  printf '%s\n' "${FAKE_PSQL_CHECK:-present}"
  exit 0
fi
cat >"$FAKE_PSQL_STDIN"
exit 0
`

// resetPodScript returns the container script of the one-shot pod whose
// generateName matches.
func resetPodScript(t *testing.T, manifests, generateName string) string {
	t.Helper()
	for _, document := range resetPodDocuments(manifests) {
		pod := yamlMap(t, document)
		metadata := mapAt(t, pod, "metadata")
		if metadata["generateName"] != generateName {
			continue
		}
		containers := listAt(t, mapAt(t, pod, "spec"), "containers")
		if len(containers) != 1 {
			t.Fatalf("pod %s has %d containers; want one", generateName, len(containers))
		}
		container, ok := containers[0].(map[string]any)
		if !ok {
			t.Fatalf("pod %s container is %T; want a map", generateName, containers[0])
		}
		args := listAt(t, container, "args")
		if len(args) != 1 {
			t.Fatalf("pod %s has %d args; want one script", generateName, len(args))
		}
		script, ok := args[0].(string)
		if !ok {
			t.Fatalf("pod %s arg is %T; want the script text", generateName, args[0])
		}
		return script
	}
	t.Fatalf("no one-shot pod with generateName %s:\n%s", generateName, manifests)
	return ""
}

type resetPodResult struct {
	code   int
	output string
	log    string
	stdin  string
}

// runResetPodScript executes one captured database pod script with a fake psql
// on PATH. The URLs carry distinct database names so the test can prove which
// one became PGDATABASE.
func runResetPodScript(t *testing.T, script string, extraEnv ...string) resetPodResult {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	writeExecutable(t, filepath.Join(bin, "psql"), fakeStageResetPsql)

	log := filepath.Join(root, "psql.log")
	stdin := filepath.Join(root, "psql.stdin")
	command := exec.Command("/bin/bash", "-ec", script)
	command.Env = append([]string{
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"KODAMA_ADMIN_DATABASE_URL=postgres://stage_admin:admin-secret@db.example:5432/rdb?sslmode=verify-full",
		"KODAMA_DATABASE_URL=postgres://kodama_runtime:runtime-secret@db.example:5432/kodama?sslmode=verify-full",
		"FAKE_PSQL_LOG=" + log,
		"FAKE_PSQL_STDIN=" + stdin,
	}, extraEnv...)

	output, err := command.CombinedOutput()
	result := resetPodResult{output: string(output)}
	if err != nil {
		exitError, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run database pod script: %v\n%s", err, output)
		}
		result.code = exitError.ExitCode()
	}
	if body, err := os.ReadFile(log); err == nil {
		result.log = string(body)
	}
	if body, err := os.ReadFile(stdin); err == nil {
		result.stdin = string(body)
	}
	return result
}

// TestStageResetConnectsToTheApplicationDatabase proves both database pods read
// the application database from the kodama-database Secret, export it as
// PGDATABASE and never connect to the database named by the admin URL.
func TestStageResetConnectsToTheApplicationDatabase(t *testing.T) {
	box := newResetSandbox(t)
	code, output := box.run(t)
	if code != 0 {
		t.Fatalf("stage-reset apply failed: exit %d\n%s", code, output)
	}
	manifests, err := os.ReadFile(box.kubectlIn)
	if err != nil {
		t.Fatalf("read pod manifests: %v", err)
	}
	text := string(manifests)
	secretRef := "secretKeyRef: {name: kodama-database, key: database_url}"
	if got := strings.Count(text, secretRef); got != 2 {
		t.Errorf("kodama-database/database_url secretKeyRef appears %d times; want the pre-state and reset pods:\n%s", got, text)
	}

	for _, generateName := range []string{
		"kodama-stage-reset-preflight-",
		"kodama-stage-reset-database-",
	} {
		script := resetPodScript(t, text, generateName)
		if strings.Contains(script, "PGDATABASE=\"${BASH_REMATCH[6]}\"") {
			t.Errorf("%s still exports PGDATABASE from the admin URL:\n%s", generateName, script)
		}
		result := runResetPodScript(t, script)
		if result.code != 0 {
			t.Fatalf("%s script failed: exit %d\n%s", generateName, result.code, result.output)
		}
		if !strings.Contains(result.log, "PGDATABASE\tkodama") {
			t.Errorf("%s connected with a database other than the application one:\n%s", generateName, result.log)
		}
		if strings.Contains(result.log, "PGDATABASE\trdb") {
			t.Errorf("%s connected to the admin URL's database:\n%s", generateName, result.log)
		}
		for line := range strings.SplitSeq(result.log, "\n") {
			if strings.HasPrefix(line, "PGDATABASE\t") {
				t.Logf("%s psql %s", generateName, line)
			}
		}
	}
	// The resolved name is pre-state evidence, not operator guesswork.
	preflight := resetPodScript(t, text, "kodama-stage-reset-preflight-")
	result := runResetPodScript(t, preflight)
	for line := range strings.SplitSeq(result.output, "\n") {
		if strings.HasPrefix(line, "resolved_database=") {
			t.Logf("pre-state evidence %s", line)
		}
	}
	if !strings.Contains(result.output, "resolved_database=kodama") {
		t.Errorf("pre-state evidence does not record the resolved database name:\n%s", result.output)
	}
	if !strings.Contains(preflight, "resolved_database=") {
		t.Errorf("pre-state pod does not record the resolved database name:\n%s", preflight)
	}

	// No stage script may assume the admin URL's database name.
	body, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "stage-reset"))
	if err != nil {
		t.Fatalf("read scripts/stage-reset: %v", err)
	}
	if regexp.MustCompile(`\brdb\b`).Match(body) {
		t.Error("scripts/stage-reset assumes the admin database name")
	}
}

// TestStageResetRefusesWithoutMigrationLedger proves both database pods refuse
// before any mutating statement when the target database has no
// kodama_meta.schema_migrations ledger (the empty admin database symptom).
func TestStageResetRefusesWithoutMigrationLedger(t *testing.T) {
	box := newResetSandbox(t)
	code, output := box.run(t)
	if code != 0 {
		t.Fatalf("stage-reset apply failed: exit %d\n%s", code, output)
	}
	manifests, err := os.ReadFile(box.kubectlIn)
	if err != nil {
		t.Fatalf("read pod manifests: %v", err)
	}
	text := string(manifests)
	for _, generateName := range []string{
		"kodama-stage-reset-preflight-",
		"kodama-stage-reset-database-",
	} {
		script := resetPodScript(t, text, generateName)
		result := runResetPodScript(t, script, "FAKE_PSQL_CHECK=absent")
		if result.code == 0 {
			t.Errorf("%s accepted a database without kodama_meta.schema_migrations:\n%s", generateName, result.output)
		}
		if !strings.Contains(result.output, "schema_migrations") {
			t.Errorf("%s refusal does not name the missing ledger:\n%s", generateName, result.output)
		}
		for _, forbidden := range []string{"DROP SCHEMA", "ALTER DATABASE", "DROP ROLE"} {
			if strings.Contains(result.stdin, forbidden) {
				t.Errorf("%s ran %q before refusing:\n%s", generateName, forbidden, result.stdin)
			}
		}
		if strings.Contains(result.log, "ALTER DATABASE") || strings.Contains(result.log, "DROP SCHEMA") {
			t.Errorf("%s passed mutating SQL to psql before refusing:\n%s", generateName, result.log)
		}
	}
}

// TestStageResetDatabaseOverrideMustMatchSecret proves an explicit
// KODAMA_STAGE_DATABASE_NAME is bounded, reaches both pods, and is refused
// when it disagrees with the Secret's database_url.
func TestStageResetDatabaseOverrideMustMatchSecret(t *testing.T) {
	t.Run("unbounded override refused before mutation", func(t *testing.T) {
		box := newResetSandbox(t).withEnv("KODAMA_STAGE_DATABASE_NAME=bad name;drop")
		code, output := box.run(t)
		if code == 0 {
			t.Fatalf("stage-reset accepted an unbounded database override:\n%s", output)
		}
		if !strings.Contains(output, "KODAMA_STAGE_DATABASE_NAME") {
			t.Errorf("refusal does not mention KODAMA_STAGE_DATABASE_NAME:\n%s", output)
		}
		if line := mutationCall(box.kubectlCalls(t)); line != "" {
			t.Errorf("stage-reset mutated before refusing:\n%s", line)
		}
		if _, err := os.Stat(box.evidence); !os.IsNotExist(err) {
			t.Errorf("a refused run created the evidence directory")
		}
	})

	t.Run("matching override reaches both pods", func(t *testing.T) {
		box := newResetSandbox(t).withEnv("KODAMA_STAGE_DATABASE_NAME=kodama")
		code, output := box.run(t)
		if code != 0 {
			t.Fatalf("stage-reset apply failed: exit %d\n%s", code, output)
		}
		manifests, err := os.ReadFile(box.kubectlIn)
		if err != nil {
			t.Fatalf("read pod manifests: %v", err)
		}
		if got := strings.Count(string(manifests), "{name: KODAMA_STAGE_DATABASE_NAME, value: 'kodama'}"); got != 2 {
			t.Errorf("the override reaches %d database pods; want 2:\n%s", got, manifests)
		}
	})

	t.Run("disagreeing override refused by the pods", func(t *testing.T) {
		box := newResetSandbox(t)
		code, output := box.run(t)
		if code != 0 {
			t.Fatalf("stage-reset apply failed: exit %d\n%s", code, output)
		}
		manifests, err := os.ReadFile(box.kubectlIn)
		if err != nil {
			t.Fatalf("read pod manifests: %v", err)
		}
		text := string(manifests)
		for _, generateName := range []string{
			"kodama-stage-reset-preflight-",
			"kodama-stage-reset-database-",
		} {
			script := resetPodScript(t, text, generateName)
			result := runResetPodScript(t, script, "KODAMA_STAGE_DATABASE_NAME=other")
			if result.code == 0 {
				t.Errorf("%s accepted an override that disagrees with the Secret:\n%s", generateName, result.output)
			}
			if !strings.Contains(result.output, "KODAMA_STAGE_DATABASE_NAME") {
				t.Errorf("%s refusal does not mention the override:\n%s", generateName, result.output)
			}
			if result.log != "" {
				t.Errorf("%s connected to a database before refusing:\n%s", generateName, result.log)
			}
		}
	})
}
