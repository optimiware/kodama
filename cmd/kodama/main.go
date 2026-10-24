package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/optimiware/kodama/internal/app"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := app.Main(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
