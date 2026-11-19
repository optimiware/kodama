package runtime_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/optimiware/kodema/internal/shell/workspace"
)

// The launcher both container runtimes start a workload through is rendered by
// internal/shell/workspace. These tests run it under the host's shell.

// launcherRun is one run of the rendered launcher under the host's shell. The
// in-container paths are replaced by a temporary toolbox directory and script,
// so the shell rules themselves are exercised: the PATH the script sees, what
// runs after it and the exit status.
type launcherRun struct {
	// path is the PATH the script observed; pushed reports that the fake
	// toolbox ran `output push`.
	path   string
	pushed bool
	status int
}

func runLauncher(t *testing.T, launch workspace.Launch, script string, pushStatus int, env []string) launcherRun {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh is not available")
	}
	directory := t.TempDir()
	toolbox := filepath.Join(directory, "bin")
	if err := os.Mkdir(toolbox, 0o755); err != nil {
		t.Fatal(err)
	}
	pushedMarker := filepath.Join(directory, "pushed")
	pathMarker := filepath.Join(directory, "path")
	fake := "#!/bin/sh\necho \"$*\" > " + pushedMarker + "\nexit " + string(rune('0'+pushStatus)) + "\n"
	if err := os.WriteFile(filepath.Join(toolbox, workspace.ToolboxName), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(directory, "script")
	body := "#!/bin/sh\nset -e\nprintf '%s' \"$PATH\" > " + pathMarker + "\n" + script + "\n"
	if err := os.WriteFile(scriptPath, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	entrypoint := workspace.Entrypoint(scriptPath, launch)
	for index := range entrypoint {
		entrypoint[index] = strings.ReplaceAll(entrypoint[index], workspace.ToolboxRoot, toolbox)
	}
	command := exec.Command(entrypoint[0], entrypoint[1:]...)
	command.Env = env
	run := launcherRun{}
	if err := command.Run(); err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("launcher did not run: %v", err)
		}
		run.status = exit.ExitCode()
	}
	if data, err := os.ReadFile(pathMarker); err == nil {
		run.path = strings.ReplaceAll(string(data), toolbox, workspace.ToolboxRoot)
	}
	if data, err := os.ReadFile(pushedMarker); err == nil {
		run.pushed = true
		if got := strings.TrimSpace(string(data)); got != "output push" {
			t.Fatalf("the toolbox ran %q; want `output push`", got)
		}
	}
	return run
}

// TestLauncherAppendsTheToolboxToTheImagePath pins that the toolbox comes
// last, behind the image's own PATH. Without one, a shell may supply its own
// default; whatever it is, the result never holds an empty element, which a
// shell reads as the current directory.
func TestLauncherAppendsTheToolboxToTheImagePath(t *testing.T) {
	run := runLauncher(t, workspace.Launch{Toolbox: true}, "true", 0, []string{"PATH=/usr/local/bin:/usr/bin:/bin"})
	if run.status != 0 {
		t.Fatalf("exit status = %d; want 0", run.status)
	}
	if want := "/usr/local/bin:/usr/bin:/bin:/kodema/bin"; run.path != want {
		t.Fatalf("the script saw PATH %q; want %q", run.path, want)
	}
	for name, env := range map[string][]string{
		"an image without a PATH":     {},
		"an image with an empty PATH": {"PATH="},
	} {
		t.Run(name, func(t *testing.T) {
			run := runLauncher(t, workspace.Launch{Toolbox: true}, "true", 0, env)
			if run.status != 0 {
				t.Fatalf("exit status = %d; want 0", run.status)
			}
			elements := strings.Split(run.path, ":")
			if elements[len(elements)-1] != workspace.ToolboxRoot {
				t.Fatalf("the script saw PATH %q; want the toolbox last", run.path)
			}
			if slices.Contains(elements, "") {
				t.Fatalf("the script saw PATH %q; want no empty element", run.path)
			}
		})
	}
}

// TestLauncherPushesOutputsOnlyAfterASucceededScript pins the launcher's whole
// rule: the push runs when the action declares outputs and the script exited
// zero, an early `exit 0` included; a failed script keeps its own status and
// pushes nothing; a failed push fails the action.
func TestLauncherPushesOutputsOnlyAfterASucceededScript(t *testing.T) {
	env := []string{"PATH=/usr/bin:/bin"}
	for _, testCase := range []struct {
		name       string
		launch     workspace.Launch
		script     string
		pushStatus int
		wantPushed bool
		wantStatus int
	}{
		{"outputs and a succeeded script", workspace.Launch{Toolbox: true, Outputs: true}, "true", 0, true, 0},
		{"outputs and a script that exits zero early", workspace.Launch{Toolbox: true, Outputs: true}, "exit 0\nfalse", 0, true, 0},
		{"outputs and a failed script", workspace.Launch{Toolbox: true, Outputs: true}, "exit 7", 0, false, 7},
		{"outputs and a failing command under errexit", workspace.Launch{Toolbox: true, Outputs: true}, "false\ntrue", 0, false, 1},
		{"outputs and a refused push", workspace.Launch{Toolbox: true, Outputs: true}, "true", 3, true, 3},
		{"no outputs and a succeeded script", workspace.Launch{Toolbox: true}, "true", 0, false, 0},
		{"no outputs and a failed script", workspace.Launch{Toolbox: true}, "exit 7", 0, false, 7},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			run := runLauncher(t, testCase.launch, testCase.script, testCase.pushStatus, env)
			if run.pushed != testCase.wantPushed {
				t.Fatalf("output push ran = %t; want %t", run.pushed, testCase.wantPushed)
			}
			if run.status != testCase.wantStatus {
				t.Fatalf("exit status = %d; want %d", run.status, testCase.wantStatus)
			}
		})
	}
}
