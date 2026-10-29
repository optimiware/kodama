package mcpserver

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
)

const usageText = `Usage: kodama mcp serve [flags]

Run the external MCP server for the Kodama public API. It is an OAuth resource
server: it verifies the caller's bearer token and forwards it to Kodama. It has
no database, broker, object-store or KMS credential.

Use "kodama mcp serve --help" for flags.`

// Command runs the `kodama mcp` command group.
func Command(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintln(stdout, usageText)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	if args[0] != "serve" {
		fmt.Fprintf(stderr, "kodama mcp: unknown command %q\n", args[0])
		fmt.Fprintln(stderr, usageText)
		return 2
	}
	cfg, err := ParseServeConfig(args[1:], getenv, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(stderr, "kodama mcp: %v\n", err)
		return 2
	}
	server, err := New(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "kodama mcp: %v\n", err)
		return 2
	}
	logger := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := server.Run(ctx, logger); err != nil {
		logger.Error("mcp server stopped", "error", err)
		return 1
	}
	return 0
}
