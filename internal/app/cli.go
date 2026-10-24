package app

import (
	"net/http"

	"github.com/optimiware/kodama/internal/cli/agent"
	"github.com/optimiware/kodama/internal/cli/auth"
	"github.com/optimiware/kodama/internal/cli/configuration"
	"github.com/optimiware/kodama/internal/cli/deployment"
	"github.com/optimiware/kodama/internal/cli/environment"
	"github.com/optimiware/kodama/internal/cli/gate"
	clipipeline "github.com/optimiware/kodama/internal/cli/pipeline"
	"github.com/optimiware/kodama/internal/cli/root"
	clirun "github.com/optimiware/kodama/internal/cli/run"
	"github.com/optimiware/kodama/internal/cli/work"
	"github.com/optimiware/kodama/internal/cli/workspace"
	"github.com/optimiware/kodama/internal/pipeline"
)

// cliGroups are the command groups the assembly supports. The CLI never offers
// a command whose route the server would answer with 404: `run logs` is offered
// since the execution assembly (T-071) serves the log API, and the agent
// (profiles), gate, environment and deployment groups since the manual delivery
// assembly (T-072) serves their routes. The model settings and MCP registrations
// of the M2 composition (T-039) are administered through the HTTP API and the
// generated client; no CLI group exists for them, for the Git trigger (installed
// through the Go-level management seam until its route is specified), for the
// business dependency links, the QA mapping (a control-plane call with no route)
// or ChatOps destinations (T-075 serves their HTTP routes only), so none is
// offered.
var cliGroups = []string{"context", "auth", "work", "workspace", "pipeline", "run", "variable", "secret", "agent", "gate", "environment", "deployment"}

// unsupportedCommands lists commands of a registered group that the assembly
// does not serve (none at present).
var unsupportedCommands = map[string]map[string]bool{}

// CLIConfig wires the command-line client. Nothing here reaches the server
// process: the CLI is a client of the public API.
type CLIConfig struct {
	Streams root.Streams
	Getenv  func(string) string
	HTTP    *http.Client
}

// NewCLI builds the runner for the supported command groups.
//
// Local state: contexts are stored at KODAMA_CONTEXT_FILE (default
// <user config>/kodama/contexts.json); credentials use the OS keyring unless a
// command selects `--credential-store file`, whose directory is
// KODAMA_CREDENTIAL_DIR (default <user config>/kodama/credentials). Plain HTTP
// to a loopback factory is accepted only with KODAMA_ALLOW_INSECURE_LOOPBACK=true
// (local development).
func NewCLI(cfg CLIConfig) (*root.Runner, error) {
	getenv := cfg.Getenv
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	ctxPath := getenv("KODAMA_CONTEXT_FILE")
	if ctxPath == "" {
		p, err := root.DefaultContextPath()
		if err != nil {
			return nil, err
		}
		ctxPath = p
	}
	insecure := getenv("KODAMA_ALLOW_INSECURE_LOOPBACK") == "true"
	authCfg := auth.ServiceConfig{HTTP: cfg.HTTP, AllowInsecureLoopbackEndpoints: insecure}
	if dir := getenv("KODAMA_CREDENTIAL_DIR"); dir != "" {
		authCfg.File = auth.NewFileStore(dir)
	}
	authSvc := auth.NewService(authCfg)

	// Local validation accepts the step kinds the server accepts (containers,
	// agent steps and gates); profile and environment references are judged by the
	// server, which holds them.
	validator, err := pipeline.NewValidator(pipeline.Config{Capabilities: pipeline.EnabledKinds{pipeline.KindContainer, pipeline.KindAgent, pipeline.KindGate}})
	if err != nil {
		return nil, err
	}
	pipelineCmds, err := clipipeline.NewCommands(clipipeline.Config{Validator: validator, Actor: pipeline.Actor{ID: "local-validation", Type: pipeline.ActorUser}})
	if err != nil {
		return nil, err
	}
	runCmds, err := clirun.NewCommands(clirun.Config{Tokens: authSvc, HTTP: cfg.HTTP, AllowInsecureLoopback: insecure})
	if err != nil {
		return nil, err
	}
	var kept []root.Command
	for _, c := range runCmds.Commands {
		if !unsupportedCommands["run"][c.Name] {
			kept = append(kept, c)
		}
	}
	runCmds.Commands = kept

	return root.NewRunner(root.RunnerConfig{
		Streams: cfg.Streams, Contexts: root.NewFileContextStore(ctxPath), Tokens: authSvc, HTTP: cfg.HTTP, AllowInsecureLoopback: insecure,
	},
		root.NewContextCommands(), auth.NewCommands(authSvc), work.NewCommands(), workspace.NewCommands(),
		pipelineCmds, runCmds, configuration.NewVariableCommands(), configuration.NewSecretCommands(),
		agent.NewCommands(), gate.NewCommands(), environment.NewCommands(), deployment.NewCommands(),
	)
}

// isCLIGroup reports whether the first argument selects a CLI group.
func isCLIGroup(name string) bool {
	for _, g := range cliGroups {
		if g == name {
			return true
		}
	}
	return false
}
