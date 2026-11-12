//go:build integration

package composition

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/userland/cli"
)

// TestKodactlRunsAgainstTheComposedApp drives one parameterized kodactl command
// through cli.Run against the served composition. The command uses the public
// client transport, so it also proves the canonical route-field expansion.
func TestKodactlRunsAgainstTheComposedApp(t *testing.T) {
	harness := newHarness(t)
	environment := harness.start(newFakeRuntime(modeComplete))
	environment.grantEngineer("admin-1", projectID)

	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), cli.Config{
		ServerURL: environment.baseURL,
		Output:    cli.OutputJSON,
	}, []string{
		"call", "task.inbox",
		`--input={"project_id":"project-1"}`,
	}, cli.Ports{
		Client: environment.transport(projectToken),
		Out:    &stdout,
		Err:    &stderr,
	})
	if code != cli.ExitSuccess {
		t.Fatalf("kodactl exit = %d; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"items"`) {
		t.Fatalf("kodactl output = %q; want the inbox result", stdout.String())
	}
}

// TestComposedAppExposesItsPublicHandler pins the one surface the command
// entrypoints consume from the composition: the app they build exposes a public
// handler. The fixture adds the rest: start waits until the served handler
// answers MCP tools/list, and its cleanup fails the test unless Serve returns
// after cancellation and Wait reports no error.
//
// The commands themselves are not driven here. Both cmd/kodama and cmd/kodactl
// are `package main` and expose no importable Run(ctx, args, io) seam, so this
// suite exercises the same app.New entry they construct; their dispatch
// behavior is covered by the cmd packages' own tests.
func TestComposedAppExposesItsPublicHandler(t *testing.T) {
	harness := newHarness(t)
	environment := harness.start(newFakeRuntime(modeComplete))
	if environment.app.Handler() == nil {
		t.Fatal("the composed app exposes no public handler")
	}
}
