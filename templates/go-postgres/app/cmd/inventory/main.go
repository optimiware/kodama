// Command inventory serves the inventory-exception API, runs the scoped ERP
// worker, applies migrations and seeds explicit test data.
//
//	inventory serve   --config api.json
//	inventory worker  --config worker.json
//	inventory migrate --config migrate.json
//	inventory seed    --config migrate.json --file seed.json
//	inventory version
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"example.invalid/inventory/internal/app"
	"example.invalid/inventory/internal/config"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

// provisioningWait bounds how long a worker waits for its environment to be
// migrated and provisioned before it exits and lets the platform restart it.
const provisioningWait = 15 * time.Minute

const usage = "usage: inventory serve|worker|migrate|seed|version --config FILE [--file SEED]"

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	cmd := args[0]
	if cmd == "version" {
		fmt.Println(version)
		return 0
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	cfgPath := fs.String("config", "", "configuration file")
	seedPath := fs.String("file", "", "seed file (seed only)")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || *cfgPath == "" {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd {
	case "serve":
		var cfg config.API
		if cfg, err = config.LoadAPI(*cfgPath); err != nil {
			return bad(err)
		}
		var a *app.API
		if a, err = app.NewAPI(ctx, cfg, log); err == nil {
			log.Info("serving", "addr", cfg.HTTPAddr, "version", version)
			err = a.Run(ctx)
		}
	case "worker":
		var cfg config.Worker
		if cfg, err = config.LoadWorker(*cfgPath); err != nil {
			return bad(err)
		}
		var w *app.Worker
		if w, err = app.AwaitWorker(ctx, cfg, log, provisioningWait, 5*time.Second); err == nil {
			log.Info("worker started", "tenant", cfg.Tenant, "service", cfg.ServiceID, "version", version)
			err = w.Run(ctx)
		}
	case "migrate":
		var cfg config.Migrate
		if cfg, err = config.LoadMigrate(*cfgPath); err != nil {
			return bad(err)
		}
		err = app.Migrate(ctx, cfg, log)
	case "seed":
		var cfg config.Migrate
		if cfg, err = config.LoadMigrate(*cfgPath); err != nil {
			return bad(err)
		}
		var raw []byte
		var seed app.SeedFile
		if raw, err = os.ReadFile(*seedPath); err == nil {
			if err = json.Unmarshal(raw, &seed); err == nil {
				err = app.Seed(ctx, cfg, seed)
			}
		}
	default:
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	if err != nil {
		log.Error(cmd+" failed", "error", err.Error())
		return 1
	}
	return 0
}

func bad(err error) int {
	fmt.Fprintln(os.Stderr, err)
	return 2
}
