// Package root is the CLI's shared foundation: common flags, saved contexts,
// the exit-code and output contract, and a small runner that dispatches
// `kodama <group> <command>`. Product commands live in other packages and
// contribute themselves as Contributions; the central binary (T-027) decides
// which contributions to register. Nothing here calls os.Exit or touches the
// process streams directly.
package root

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/optimiware/kodama/internal/cli/client"
)

// TokenResolver is the consumer-owned port that yields an access token source
// for an effective context. The implementation must bind lookup to the
// context's CredentialKey and selected credential store, and must never start
// an interactive login.
type TokenResolver interface {
	TokenSource(ec EffectiveContext) (client.AccessTokenSource, error)
}

// RunnerConfig wires a Runner; all adapters are injected by the composition root.
type RunnerConfig struct {
	Streams  Streams
	Contexts ContextStore
	Tokens   TokenResolver // nil: Invocation.Client fails
	// HTTP is copied for API calls; nil uses a default client.
	HTTP *http.Client
	// AllowInsecureLoopback permits http:// loopback factories (development, tests).
	AllowInsecureLoopback bool
}

// Invocation is what a command receives.
type Invocation struct {
	Streams   Streams
	Flags     *Flags
	Overrides Overrides
	// Context is the effective context for commands declaring NeedContext or
	// NeedFactory; it is zero for NeedNone.
	Context  EffectiveContext
	Format   Format
	Contexts ContextStore

	tokens   TokenResolver
	http     *http.Client
	insecure bool
}

// Client builds the API client for the effective factory and credential.
func (inv *Invocation) Client() (*client.Client, error) {
	if err := inv.Context.RequireFactory(); err != nil {
		return nil, err
	}
	if inv.tokens == nil {
		return nil, NewError(ExitFailure, "no_credential_resolver", "no credential resolver is configured")
	}
	src, err := inv.tokens.TokenSource(inv.Context)
	if err != nil {
		return nil, err
	}
	return client.New(client.Config{
		BaseURL: inv.Context.Factory, AllowInsecureLoopback: inv.insecure, Tokens: src, HTTP: inv.http,
	})
}

// Need says how much of the effective context a command requires.
type Need int

const (
	// NeedNone: the command manages contexts itself and resolves nothing.
	NeedNone Need = iota
	// NeedContext: resolve the effective context; a factory is optional.
	NeedContext
	// NeedFactory: resolve the effective context and require a factory.
	NeedFactory
)

// RunFunc executes a parsed command. args are the non-flag arguments.
type RunFunc func(ctx context.Context, inv *Invocation, args []string) (Result, error)

// Command is one `group name` command.
type Command struct {
	Name    string
	Summary string
	Need    Need
	// Bind registers command-specific flags on fs (after the common flags) and
	// returns the function to run once parsing and context resolution succeed.
	Bind func(fs *flag.FlagSet) RunFunc
}

// Contribution is a group of commands supplied by one package.
type Contribution struct {
	Group    string
	Summary  string
	Commands []Command
}

// Runner dispatches contributions.
type Runner struct {
	cfg    RunnerConfig
	groups map[string]Contribution
}

// NewRunner registers contributions; duplicate group/command names fail.
func NewRunner(cfg RunnerConfig, contributions ...Contribution) (*Runner, error) {
	if cfg.Streams.In == nil {
		cfg.Streams.In = strings.NewReader("")
	}
	if cfg.Streams.Out == nil {
		cfg.Streams.Out = io.Discard
	}
	if cfg.Streams.Err == nil {
		cfg.Streams.Err = io.Discard
	}
	r := &Runner{cfg: cfg, groups: map[string]Contribution{}}
	for _, c := range contributions {
		g, ok := r.groups[c.Group]
		if !ok {
			g = Contribution{Group: c.Group, Summary: c.Summary}
		}
		for _, cmd := range c.Commands {
			if cmd.Name == "" || cmd.Bind == nil || c.Group == "" {
				return nil, fmt.Errorf("root: contribution %q has an incomplete command", c.Group)
			}
			for _, have := range g.Commands {
				if have.Name == cmd.Name {
					return nil, fmt.Errorf("root: duplicate command %s %s", c.Group, cmd.Name)
				}
			}
			g.Commands = append(g.Commands, cmd)
		}
		if g.Summary == "" {
			g.Summary = c.Summary
		}
		r.groups[c.Group] = g
	}
	return r, nil
}

// Run executes args (without the program name) and returns the exit code.
// Success output goes only to stdout, failure output only to stderr.
func (r *Runner) Run(ctx context.Context, args []string) ExitCode {
	s := r.cfg.Streams
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		_, _ = io.WriteString(s.Out, r.usage())
		if len(args) == 0 {
			return ExitUsage
		}
		return ExitOK
	}
	g, ok := r.groups[args[0]]
	if !ok {
		return r.fail(peekOutput(args), Usagef("unknown command group %q (see `kodama help`)", args[0]))
	}
	if len(args) < 2 || args[1] == "-h" || args[1] == "--help" || args[1] == "help" {
		_, _ = io.WriteString(s.Out, r.groupUsage(g))
		if len(args) < 2 {
			return ExitUsage
		}
		return ExitOK
	}
	var cmd *Command
	for i := range g.Commands {
		if g.Commands[i].Name == args[1] {
			cmd = &g.Commands[i]
		}
	}
	if cmd == nil {
		return r.fail(peekOutput(args), Usagef("unknown command %q in group %q", args[1], g.Group))
	}

	fs := flag.NewFlagSet(g.Group+" "+cmd.Name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	flags := BindFlags(fs)
	run := cmd.Bind(fs)
	positional, err := parseInterspersed(fs, args[2:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(s.Out)
			fmt.Fprintf(s.Out, "usage: kodama %s %s [flags]\n\n%s\n\n", g.Group, cmd.Name, cmd.Summary)
			fs.PrintDefaults()
			return ExitOK
		}
		return r.fail(peekOutput(args), Usagef("%v", err))
	}
	o := flags.Overrides()
	format := FormatHuman
	if o.Output != nil {
		f, err := ParseFormat(*o.Output)
		if err != nil {
			return r.fail(peekOutput(args), err)
		}
		format = f
	}
	inv := &Invocation{
		Streams: s, Flags: flags, Overrides: o, Format: format, Contexts: r.cfg.Contexts,
		tokens: r.cfg.Tokens, http: r.cfg.HTTP, insecure: r.cfg.AllowInsecureLoopback,
	}
	if err := o.validate(); err != nil {
		return r.fail(format, err)
	}
	if o.Timeout != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *o.Timeout)
		defer cancel()
	}
	// A command never starts side effects under a context that is already
	// over: report the timeout or interruption, with nothing persisted.
	if err := ctx.Err(); err != nil {
		return r.fail(format, err)
	}
	if cmd.Need != NeedNone {
		eff, err := ResolveContext(ctx, r.cfg.Contexts, o)
		if err != nil {
			return r.fail(format, err)
		}
		if cmd.Need == NeedFactory {
			if err := eff.RequireFactory(); err != nil {
				return r.fail(format, err)
			}
		}
		inv.Context = eff
	}
	res, err := run(ctx, inv, positional)
	if err != nil {
		return r.fail(format, err)
	}
	if err := WriteSuccess(s, format, res); err != nil {
		return r.fail(format, NewError(ExitFailure, "output_failed", "cannot write the result: %v", err))
	}
	return ExitOK
}

func (r *Runner) fail(f Format, err error) ExitCode {
	p := Classify(err)
	WriteFailure(r.cfg.Streams, f, p)
	return p.Exit
}

func (r *Runner) usage() string {
	var b strings.Builder
	b.WriteString("usage: kodama <group> <command> [flags]\n\ngroups:\n")
	names := make([]string, 0, len(r.groups))
	for n := range r.groups {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(&b, "  %-12s %s\n", n, r.groups[n].Summary)
	}
	b.WriteString("\nexit codes: 0 success, 1 remote/operation failure, 2 usage/validation, 3 authentication/authorization, 4 conflict, 5 timeout/interruption\n")
	return b.String()
}

func (r *Runner) groupUsage(g Contribution) string {
	var b strings.Builder
	fmt.Fprintf(&b, "usage: kodama %s <command> [flags]\n\ncommands:\n", g.Group)
	cmds := append([]Command(nil), g.Commands...)
	sort.Slice(cmds, func(i, j int) bool { return cmds[i].Name < cmds[j].Name })
	for _, c := range cmds {
		fmt.Fprintf(&b, "  %-12s %s\n", c.Name, c.Summary)
	}
	return b.String()
}
