package clientcmd

import (
	"bytes"
	"context"
	"slices"
	"testing"

	"github.com/optimiware/kodema/internal/userland/cli"
)

func TestWatchAndEventsExplicitProjectFlagPropagation(t *testing.T) {
	t.Run("watch_with_hoisted_project_sets_ExplicitProject", func(t *testing.T) {
		var (
			gotCfg  cli.Config
			gotArgs []string
			gotDeps cli.CommandDeps
		)
		deps := defaultCLIDeps()
		deps.runCLI = func(_ context.Context, cfg cli.Config, args []string, _ cli.Ports, cmdDeps cli.CommandDeps) int {
			gotCfg = cfg
			gotArgs = append([]string(nil), args...)
			gotDeps = cmdDeps
			return 0
		}
		var out, errOut bytes.Buffer
		code := run(context.Background(), []string{
			"--server", "https://api.kodema.example",
			"watch", "--project", "proj-live", "--until", "request",
		}, &out, &errOut, deps)
		if code != 0 {
			t.Fatalf("exit = %d; stderr=%q", code, errOut.String())
		}
		if gotCfg.Project != "proj-live" || !gotDeps.ExplicitProject {
			t.Fatalf("Project=%q ExplicitProject=%v; want proj-live, true", gotCfg.Project, gotDeps.ExplicitProject)
		}
		if !slices.Equal(gotArgs, []string{"watch", "--until", "request"}) {
			t.Fatalf("args = %v; want [watch --until request]", gotArgs)
		}
	})

	t.Run("watch_without_project_flag_leaves_ExplicitProject_false", func(t *testing.T) {
		t.Setenv("KODEMA_PROJECT", "proj-from-env")
		var gotDeps cli.CommandDeps
		deps := defaultCLIDeps()
		deps.runCLI = func(_ context.Context, _ cli.Config, _ []string, _ cli.Ports, cmdDeps cli.CommandDeps) int {
			gotDeps = cmdDeps
			return 0
		}
		var out, errOut bytes.Buffer
		code := run(context.Background(), []string{
			"--server", "https://api.kodema.example",
			"watch",
		}, &out, &errOut, deps)
		if code != 0 {
			t.Fatalf("exit = %d; stderr=%q", code, errOut.String())
		}
		if gotDeps.ExplicitProject {
			t.Fatal("ExplicitProject = true for ambient KODEMA_PROJECT; want false")
		}
	})

	t.Run("events_hoists_project_and_output_flags", func(t *testing.T) {
		var (
			gotCfg  cli.Config
			gotArgs []string
		)
		deps := defaultCLIDeps()
		deps.runCLI = func(_ context.Context, cfg cli.Config, args []string, _ cli.Ports, _ cli.CommandDeps) int {
			gotCfg = cfg
			gotArgs = append([]string(nil), args...)
			return 0
		}
		var out, errOut bytes.Buffer
		code := run(context.Background(), []string{
			"--server", "https://api.kodema.example",
			"events", "--project", "proj-1", "--type", "task.created", "--follow", "--output", "json",
		}, &out, &errOut, deps)
		if code != 0 {
			t.Fatalf("exit = %d; stderr=%q", code, errOut.String())
		}
		if gotCfg.Project != "proj-1" || gotCfg.Output != cli.OutputJSON {
			t.Fatalf("cfg = %+v", gotCfg)
		}
		if !slices.Equal(gotArgs, []string{"events", "--type", "task.created", "--follow"}) {
			t.Fatalf("args = %v; want [events --type task.created --follow]", gotArgs)
		}
	})
}
