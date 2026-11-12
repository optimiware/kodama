// Package script tests the fail-fast entrypoint script generation required by
// S07 §2 and KODAMA.md "Container actions": the adapter replaces the image
// entrypoint with a fail-fast script that preserves the declared command bytes
// and order, adds no tracing and injects no Kodama runner.
//
// The tests exercise this package surface, which runtime adapters consume:
//
//	const ShellPath = "/bin/sh"
//	func Render(commands []string) (string, error)
//
// Render returns "#!/bin/sh\nset -e\n" followed by every command joined with
// "\n" and a final newline. Commands are opaque bytes: no trimming, splitting,
// escaping, deduplication or reordering. An empty command list or a command
// containing a NUL byte is refused with invalid_input.
//
// No test starts a shell, container, process or external command.
package script

import (
	"reflect"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
)

const expectedPreamble = "#!/bin/sh\nset -e\n"

func render(t *testing.T, commands []string) string {
	t.Helper()
	got, err := Render(commands)
	if err != nil {
		t.Fatalf("Render(%q) error = %v, want nil", commands, err)
	}
	return got
}

func assertFaultClass(t *testing.T, err error, want fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want fault class %s", want)
	}
	class, ok := fault.ClassOf(err)
	if !ok {
		t.Fatalf("error %v carries no fault class, want %s", err, want)
	}
	if class != want {
		t.Fatalf("fault class = %s, want %s (error %v)", class, want, err)
	}
}

func TestShellPath(t *testing.T) {
	if ShellPath != "/bin/sh" {
		t.Fatalf("ShellPath = %q, want /bin/sh", ShellPath)
	}
}

func TestRenderFailFastPreamble(t *testing.T) {
	got := render(t, []string{"echo hi"})
	if got != expectedPreamble+"echo hi\n" {
		t.Fatalf("Render = %q, want %q", got, expectedPreamble+"echo hi\n")
	}

	lines := strings.Split(got, "\n")
	if lines[0] != "#!/bin/sh" {
		t.Fatalf("first line = %q, want #!/bin/sh", lines[0])
	}
	if lines[1] != "set -e" {
		t.Fatalf("second line = %q, want set -e", lines[1])
	}
	if count := strings.Count(got, "\nset "); count != 1 {
		t.Fatalf("script carries %d set directives, want exactly the fail-fast one: %q", count, got)
	}
	if !strings.HasPrefix(got, "#!"+ShellPath+"\n") {
		t.Fatalf("script does not start with the declared shell: %q", got)
	}
}

func TestRenderPreservesCommandBytesAndOrder(t *testing.T) {
	commands := []string{
		`echo "double quoted value"`,
		`echo 'single quoted value'`,
		`printf '%s\n' "tab\there"`,
		`echo $HOME ${PATH} $(date +%s)`,
		`test -f /workspace/out.txt && echo present`,
		`echo one; echo two | tr a-z A-Z > /workspace/log.txt`,
		`for file in *.txt; do echo "$file"; done`,
		`  echo padded  `,
		`echo "backslash \\ and dollar \$HOME"`,
		`echo "unicode: café — ok"`,
	}
	got := render(t, commands)
	want := expectedPreamble + strings.Join(commands, "\n") + "\n"
	if got != want {
		t.Fatalf("Render rewrote command bytes:\nwant %q\ngot  %q", want, got)
	}

	offset := len(expectedPreamble)
	for index, command := range commands {
		position := strings.Index(got[offset:], command)
		if position < 0 {
			t.Fatalf("command %d %q missing after offset %d in %q", index, command, offset, got)
		}
		offset += position + len(command)
	}
}

func TestRenderPreservesMultilineAndHeredocCommands(t *testing.T) {
	heredoc := "cat > /workspace/plan.md <<'EOF'\n# Plan\n\n  indented body\nEOF"
	multi := "if [ -f /workspace/plan.md ]\nthen\n  echo found\nfi"
	commands := []string{heredoc, multi}

	got := render(t, commands)
	want := expectedPreamble + strings.Join(commands, "\n") + "\n"
	if got != want {
		t.Fatalf("Render rewrote multiline commands:\nwant %q\ngot  %q", want, got)
	}
	if !strings.Contains(got, "\n  indented body\n") {
		t.Fatalf("heredoc body indentation lost: %q", got)
	}
	if !strings.Contains(got, "\nEOF\n") {
		t.Fatalf("heredoc terminator lost: %q", got)
	}
	if !strings.Contains(got, "\nthen\n  echo found\nfi\n") {
		t.Fatalf("multiline command rewritten: %q", got)
	}
}

func TestRenderInjectsNoTracingOrRunner(t *testing.T) {
	got := render(t, []string{"echo one", "echo two"})
	forbidden := []string{
		"set -x", "set +e", "set -u", "set -o", "pipefail", "xtrace", "PS4",
		"bash", "shopt", "[[", "local ", "function ", "source ", "$'", "<<<", "&>", "|&",
		"trap", "eval ", "exec ",
		"kodama", "kodactl", "runner",
		"#!/usr/bin/env",
	}
	for _, token := range forbidden {
		if strings.Contains(got, token) {
			t.Fatalf("script contains injected or nonportable token %q: %q", token, got)
		}
	}
}

func TestRenderRefusesEmptyCommands(t *testing.T) {
	for _, commands := range [][]string{nil, {}} {
		got, err := Render(commands)
		assertFaultClass(t, err, fault.InvalidInput)
		if got != "" {
			t.Fatalf("Render(%q) = %q, want empty output on refusal", commands, got)
		}
	}
}

func TestRenderRefusesNULBytes(t *testing.T) {
	cases := [][]string{
		{"echo a\x00b"},
		{"\x00"},
		{"echo ok", "bad\x00"},
	}
	for _, commands := range cases {
		got, err := Render(commands)
		assertFaultClass(t, err, fault.InvalidInput)
		if got != "" {
			t.Fatalf("Render(%q) = %q, want empty output on refusal", commands, got)
		}
	}
}

func TestRenderPreservesEmptyAndWhitespaceCommands(t *testing.T) {
	commands := []string{"", "  ", "\t", "echo last"}
	got := render(t, commands)
	want := expectedPreamble + "\n  \n\t\necho last\n"
	if got != want {
		t.Fatalf("Render trimmed or dropped commands:\nwant %q\ngot  %q", want, got)
	}
}

func TestRenderDoesNotAddTrailingOrLeadingBytes(t *testing.T) {
	got := render(t, []string{"echo end  "})
	if !strings.HasSuffix(got, "echo end  \n") {
		t.Fatalf("trailing spaces or newline lost: %q", got)
	}
	if strings.HasSuffix(got, "\n\n") {
		t.Fatalf("Render added a trailing blank line: %q", got)
	}
	if strings.HasPrefix(got, "\n") || strings.HasPrefix(got, "\ufeff") {
		t.Fatalf("Render added a leading newline or BOM: %q", got)
	}
}

func TestRenderPreservesCarriageReturns(t *testing.T) {
	commands := []string{"printf 'a\r\n'", "echo after"}
	got := render(t, commands)
	want := expectedPreamble + strings.Join(commands, "\n") + "\n"
	if got != want {
		t.Fatalf("Render rewrote CR bytes:\nwant %q\ngot  %q", want, got)
	}
}

func TestRenderIsDeterministicAndDoesNotMutateInput(t *testing.T) {
	commands := []string{"echo one", "echo two"}
	original := append([]string(nil), commands...)

	first := render(t, commands)
	second := render(t, commands)
	if first != second {
		t.Fatalf("Render is not deterministic:\nfirst  %q\nsecond %q", first, second)
	}
	if !reflect.DeepEqual(commands, original) {
		t.Fatalf("Render mutated its input: %q, want %q", commands, original)
	}
}
