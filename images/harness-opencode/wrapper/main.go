// Command kodama-opencode-wrapper is the headless OpenCode wrapper of the
// Kodama agent runtime: it runs under `kodama step`, drives one pinned OpenCode
// version for one attempt and reports a structured result through the step
// entrypoint's results manifest. See images/harness-opencode/README.md.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	os.Exit(run(ctx, options{args: os.Args[1:], getenv: os.Getenv, stdout: os.Stdout}))
}
