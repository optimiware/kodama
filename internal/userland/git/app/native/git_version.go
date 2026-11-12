package native

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/userland/git"
)

// MinimumGitVersion is the oldest native Git the service accepts. Git 2.44.0
// is the first release with `rev-parse --show-ref-format`
// (https://github.com/git/git/blob/v2.44.0/builtin/rev-parse.c); the option is
// absent from 2.43.0
// (https://github.com/git/git/blob/v2.43.0/builtin/rev-parse.c), and the
// committed admin policy needs it to prove the repository's ref backend.
const MinimumGitVersion = "2.44.0"

// VersionProbeTimeout bounds one startup `git --version` probe when the caller's
// context carries no earlier deadline. The probe must never delay startup
// indefinitely.
const VersionProbeTimeout = 10 * time.Second

// GitVersion is one parsed numeric Git version.
type GitVersion struct {
	Major int
	Minor int
	Patch int
}

// String renders the three numeric components. Vendor suffixes are not kept.
func (version GitVersion) String() string {
	return strconv.Itoa(version.Major) + "." + strconv.Itoa(version.Minor) + "." + strconv.Itoa(version.Patch)
}

// atLeast reports whether version is at or above the minimum. The comparison is
// numeric per component: 2.100.0 is newer than 2.44.0 even though a lexical
// comparison says otherwise.
func (version GitVersion) atLeast(minimum GitVersion) bool {
	if version.Major != minimum.Major {
		return version.Major > minimum.Major
	}
	if version.Minor != minimum.Minor {
		return version.Minor > minimum.Minor
	}
	return version.Patch >= minimum.Patch
}

var minimumGitVersion = GitVersion{Major: 2, Minor: 44, Patch: 0}

// RequireGitVersion probes the exact native executable through the service's
// command executor and refuses anything older than MinimumGitVersion. The probe
// is context-aware: a cancelled startup runs no command, and a caller without a
// deadline gets VersionProbeTimeout. Unparsable, empty or failed output is a
// refusal, never an accepted default.
func RequireGitVersion(ctx context.Context, executor git.CommandExecutor) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if executor == nil {
		return fmt.Errorf("probe Git version: no command executor is wired")
	}
	if _, bounded := ctx.Deadline(); !bounded {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, VersionProbeTimeout)
		defer cancel()
	}
	result, err := executor.Run(ctx, git.Command{Name: "git", Args: []string{"--version"}})
	if err != nil {
		return fmt.Errorf("probe Git version: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("git --version failed with exit code %d: %s; Kodama requires Git %s or newer",
			result.ExitCode, strings.TrimSpace(string(result.Stderr)), MinimumGitVersion)
	}
	version, err := parseGitVersion(result.Stdout)
	if err != nil {
		return fmt.Errorf("%w; Kodama requires Git %s or newer", err, MinimumGitVersion)
	}
	if !version.atLeast(minimumGitVersion) {
		return fmt.Errorf("git version %s is unsupported: Kodama requires Git %s or newer", version, MinimumGitVersion)
	}
	return nil
}

// parseGitVersion parses the one documented `git --version` line. Git prints
// `git version <major>.<minor>.<patch>` with an optional parenthetical build
// note (Apple) or dot-separated vendor components (Windows), so only the first
// three components must be numeric.
func parseGitVersion(output []byte) (GitVersion, error) {
	text := strings.TrimSpace(string(output))
	if text == "" {
		return GitVersion{}, fmt.Errorf("git --version produced no output")
	}
	if lines := strings.Split(text, "\n"); len(lines) != 1 {
		return GitVersion{}, fmt.Errorf("git --version produced %d lines; want exactly one", len(lines))
	}
	const prefix = "git version "
	if !strings.HasPrefix(text, prefix) {
		return GitVersion{}, fmt.Errorf("git --version output %q is not a Git version", text)
	}
	fields := strings.Fields(strings.TrimPrefix(text, prefix))
	if len(fields) == 0 {
		return GitVersion{}, fmt.Errorf("git --version output %q has no version", text)
	}
	token := fields[0]
	parts := strings.Split(token, ".")
	if len(parts) < 3 {
		return GitVersion{}, fmt.Errorf("git version %q has no major.minor.patch components", token)
	}
	numbers := make([]int, 3)
	for index := 0; index < 3; index++ {
		if !decimalDigits(parts[index]) {
			return GitVersion{}, fmt.Errorf("git version %q component %q is not a number", token, parts[index])
		}
		number, err := strconv.Atoi(parts[index])
		if err != nil {
			return GitVersion{}, fmt.Errorf("git version %q component %q is out of range", token, parts[index])
		}
		numbers[index] = number
	}
	for _, vendor := range parts[3:] {
		if vendor == "" {
			return GitVersion{}, fmt.Errorf("git version %q has an empty vendor component", token)
		}
	}
	return GitVersion{Major: numbers[0], Minor: numbers[1], Patch: numbers[2]}, nil
}

// decimalDigits reports whether value is a non-empty run of ASCII digits.
func decimalDigits(value string) bool {
	if value == "" {
		return false
	}
	for index := 0; index < len(value); index++ {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}
	return true
}
