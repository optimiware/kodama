package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/optimiware/kodama/internal/id"
	gitapp "github.com/optimiware/kodama/internal/userland/git/app"
)

// provisionTenant is the injectable provisioning entrypoint. Tests replace it
// to pin flag parsing without a real repository.
var provisionTenant = gitapp.ProvisionTenant

// runGitProvisionEntry runs `kodama git provision-tenant`. It dispatches
// independently of `git serve`: it starts no listener and no hooks server.
func runGitProvisionEntry(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("git provision-tenant", flag.ContinueOnError)
	flags.SetOutput(stderr)
	tenant := flags.String("tenant", "", "tenant identity")
	storageRoot := flags.String("storage-root", "", "repository storage root")
	journalDirectory := flags.String("journal-dir", "", "receive journal directory")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		usage(stderr, fmt.Sprintf("unexpected argument %q", flags.Arg(0)))
		return 2
	}
	if strings.TrimSpace(*tenant) == "" || strings.TrimSpace(*storageRoot) == "" ||
		strings.TrimSpace(*journalDirectory) == "" {
		usage(stderr, "git provision-tenant requires --tenant, --storage-root and --journal-dir")
		return 2
	}
	if ctx == nil {
		ctx = context.Background()
	}
	result, err := provisionTenant(ctx, gitapp.ProvisionConfig{
		TenantID:         id.TenantID(*tenant),
		StorageRoot:      *storageRoot,
		JournalDirectory: *journalDirectory,
	})
	if err != nil {
		fmt.Fprintf(stderr, "kodama: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, result.RepositoryPath)
	return 0
}
