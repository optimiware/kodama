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

// provisionRepository is the injectable repository-provisioning entrypoint.
// Tests replace it to pin flag parsing and output without a real repository.
var provisionRepository = gitapp.ProvisionRepository

// runGitProvisionRepositoryEntry runs `kodama git provision-repository`. It
// dispatches independently of `git serve`: it starts no listener and no hooks
// server. The printed --resource mapping is the flag the operator adds to the
// next `git serve` invocation; a running service does not pick up a new
// repository without that restart.
func runGitProvisionRepositoryEntry(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("git provision-repository", flag.ContinueOnError)
	flags.SetOutput(stderr)
	tenant := flags.String("tenant", "", "tenant identity")
	storageRoot := flags.String("storage-root", "", "repository storage root")
	repository := flags.String("repository", "", "tenant-relative repository name")
	resource := flags.String("resource", "", "registered resource id")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		usage(stderr, fmt.Sprintf("unexpected argument %q", flags.Arg(0)))
		return 2
	}
	if strings.TrimSpace(*tenant) == "" || strings.TrimSpace(*storageRoot) == "" ||
		strings.TrimSpace(*repository) == "" || strings.TrimSpace(*resource) == "" {
		usage(stderr, "git provision-repository requires --tenant, --storage-root, --repository and --resource")
		return 2
	}
	// The mapping is echoed verbatim into the operator's next `git serve`
	// invocation, so refuse a resource id the public contract would not accept
	// (or one that would corrupt the printed --resource flag) before any effect.
	if !validResourceID(*resource) {
		usage(stderr, fmt.Sprintf("invalid --resource id %q; want 1-%d letters, digits, '-' or '_'", *resource, 128))
		return 2
	}
	if ctx == nil {
		ctx = context.Background()
	}
	result, err := provisionRepository(ctx, gitapp.RepositoryProvisionConfig{
		TenantID:    id.TenantID(*tenant),
		StorageRoot: *storageRoot,
		Repository:  *repository,
		ResourceID:  *resource,
	})
	if err != nil {
		fmt.Fprintf(stderr, "kodama: %v\n", err)
		return 1
	}
	// The app reports the canonical tenant-relative name. Falling back to the
	// operator's spelling keeps this entry testable with an injected boundary.
	name := strings.TrimSpace(result.Repository)
	if name == "" {
		name = strings.TrimSpace(*repository)
	}
	mapping := "--resource " + name + "=" + *resource
	fmt.Fprintln(stdout, mapping)
	fmt.Fprintln(stdout, "restart kodama git serve with the new --resource mapping")
	return 0
}

// validResourceID mirrors the public contract's resource-id syntax: 1 to 128
// ASCII letters, digits, '-' or '_', and never the dot segments.
func validResourceID(value string) bool {
	if value == "" || len(value) > 128 || value == "." || value == ".." {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '-', character == '_':
		default:
			return false
		}
	}
	return true
}
