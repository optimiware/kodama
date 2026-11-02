package entrypoint

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
)

// GitCredentialHelperCommandName is the subcommand Git runs as its credential
// helper for the Git service (`kodama git-credential-helper`).
const GitCredentialHelperCommandName = "git-credential-helper"

// GitUsername is the conventional user name of every Git service credential
// (the credential is the password; the service accepts any user name).
const GitUsername = "kodama"

// GitCredentialHelperCommand returns the `kodama git-credential-helper`
// registration. The composition root registers it beside StepCommand(); the
// entrypoint and the preparation configure Git to run exactly this command for
// the Git service's URLs and nothing else.
func GitCredentialHelperCommand() Command {
	return Command{
		Name:    GitCredentialHelperCommandName,
		Summary: "Git credential helper scoped to the tenant Git service (used by kodama step and kodama prepare)",
		Run: func(_ context.Context, args []string, _ func(string) string, stdout, stderr io.Writer) int {
			return RunGitCredentialHelper(args, os.Stdin, stdout, stderr)
		},
	}
}

var credentialValueRE = regexp.MustCompile(gitv1.Patterns["Credential"])

// credentialScope is where a credential file may be offered: only for https URLs
// beneath base (and, when repo is set, only for that one repository), never for
// another host, port, scheme or path.
type credentialScope struct {
	base *url.URL
	repo *url.URL // optional: the one repository URL the credential answers for
	file string
}

func parseScopeURL(raw, what string) (*url.URL, error) {
	u, err := url.Parse(raw)
	switch {
	case err != nil || u.Scheme != "https" || u.Host == "":
		return nil, fmt.Errorf("%s must be an absolute https URL", what)
	case u.User != nil || u.RawQuery != "" || u.Fragment != "":
		return nil, fmt.Errorf("%s must not carry credentials, a query or a fragment", what)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u, nil
}

// RunGitCredentialHelper is the helper process: args are
// `--base-url URL --file FILE [--repository-url URL] OPERATION` and stdin is
// Git's credential description. Only `get` ever answers; `store` and `erase`
// are accepted and do nothing (the credential file is the only store). A request
// outside the scope gets no answer at all. The credential is written to stdout
// for Git and nowhere else; nothing here logs it.
func RunGitCredentialHelper(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("kodama git-credential-helper", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var baseRaw, repoRaw, file string
	fs.StringVar(&baseRaw, "base-url", "", "")
	fs.StringVar(&repoRaw, "repository-url", "", "")
	fs.StringVar(&file, "file", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		fmt.Fprintln(stderr, "kodama git-credential-helper: usage: --base-url URL --file FILE [--repository-url URL] get|store|erase")
		return ExitUsage
	}
	base, err := parseScopeURL(baseRaw, "base URL")
	if err != nil || file == "" {
		fmt.Fprintln(stderr, "kodama git-credential-helper: a base URL and a credential file are required")
		return ExitUsage
	}
	scope := credentialScope{base: base, file: file}
	if repoRaw != "" {
		if scope.repo, err = parseScopeURL(repoRaw, "repository URL"); err != nil {
			fmt.Fprintln(stderr, "kodama git-credential-helper:", err)
			return ExitUsage
		}
	}
	switch fs.Arg(0) {
	case "store", "erase":
		_, _ = io.Copy(io.Discard, io.LimitReader(stdin, 64<<10))
		return ExitOK
	case "get":
	default:
		return ExitOK // an operation Git may add: not answered
	}
	req, err := readCredentialRequest(stdin)
	if err != nil {
		fmt.Fprintln(stderr, "kodama git-credential-helper:", err)
		return ExitUsage
	}
	if !scope.covers(req) {
		return ExitOK // refused: no username, no password
	}
	secret, err := readCredentialFile(file)
	if err != nil {
		fmt.Fprintln(stderr, "kodama git-credential-helper:", err)
		return ExitError
	}
	_, _ = fmt.Fprintf(stdout, "username=%s\npassword=%s\n", GitUsername, secret)
	return ExitOK
}

// readCredentialRequest reads Git's `key=value` lines until a blank line.
func readCredentialRequest(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(io.LimitReader(r, 64<<10))
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || k == "" {
			return nil, errors.New("malformed credential request")
		}
		out[k] = v
	}
	if err := sc.Err(); err != nil {
		return nil, errors.New("unreadable credential request")
	}
	return out, nil
}

// covers reports whether the request names a URL this scope answers for. Git
// sends the path only with credential.useHttpPath=true, which the entrypoint
// always sets: a request without a path is never answered for a service with a
// path prefix, so a credential cannot leak to a sibling path.
func (s credentialScope) covers(req map[string]string) bool {
	if req["protocol"] != "https" || req["host"] == "" {
		return false
	}
	if !strings.EqualFold(req["host"], s.base.Host) {
		return false
	}
	reqPath := "/" + req["path"]
	if req["path"] == "" && s.base.Path != "" {
		return false
	}
	if req["path"] != "" && (path.Clean(reqPath) != reqPath || strings.Contains(reqPath, "%")) {
		return false
	}
	within := func(root string) bool {
		return reqPath == root || strings.HasPrefix(reqPath, root+"/") || root == ""
	}
	if !within(s.base.Path) {
		return false
	}
	if s.repo != nil {
		return strings.EqualFold(s.repo.Host, req["host"]) && (reqPath == s.repo.Path || strings.HasPrefix(reqPath, s.repo.Path+"/"))
	}
	return true
}

// readCredentialFile returns the opaque credential: one line of the Git
// contract's credential alphabet. The value is never echoed in an error.
func readCredentialFile(file string) (string, error) {
	fi, err := os.Lstat(file)
	switch {
	case err != nil:
		return "", errors.New("the credential file cannot be read")
	case !fi.Mode().IsRegular() || fi.Size() > 4<<10:
		return "", errors.New("the credential file is not a regular file of credential size")
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return "", errors.New("the credential file cannot be read")
	}
	v := strings.TrimSpace(string(b))
	if !credentialValueRE.MatchString(v) {
		return "", errors.New("the credential file does not hold a credential")
	}
	return v, nil
}

// gitFile maps a /kodama/git/<name> path of a context onto the configured Git
// directory. Anything else is refused: the context's files lie directly under it.
func (c Config) gitFile(declared string) (string, error) { return mapGitFile(c.GitDir, declared) }

func mapGitFile(gitDir, declared string) (string, error) {
	rest, ok := strings.CutPrefix(declared, GitPrefix+"/")
	if !ok || rest == "" || strings.Contains(rest, "/") {
		return "", fmt.Errorf("%q is not a file directly under %s", declared, GitPrefix)
	}
	if gitDir == "" {
		gitDir = DefaultGitDir
	}
	return filepath.Join(gitDir, rest), nil
}

// shellQuote quotes s for the POSIX shell Git runs a `!` helper with.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// helperArgv is the argv prefix that runs this binary's credential helper.
func helperArgv(override []string) ([]string, error) {
	if len(override) > 0 {
		return override, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("cannot locate the kodama binary: %w", err)
	}
	return []string{exe, GitCredentialHelperCommandName}, nil
}

// credentialConfig is the Git configuration (as -c arguments, after the base
// configuration's reset of every inherited helper) that offers the credential in
// file to the URLs beneath base only (and to one repository when repo is set). It
// carries paths, never the credential.
func (c Config) credentialConfig(base, repo, file string) ([]string, error) {
	return credentialConfig(c.GitHelper, base, repo, file)
}

func credentialConfig(helper []string, base, repo, file string) ([]string, error) {
	argv, err := helperArgv(helper)
	if err != nil {
		return nil, err
	}
	parts := make([]string, 0, len(argv)+6)
	for _, a := range argv {
		parts = append(parts, shellQuote(a))
	}
	parts = append(parts, "--base-url", shellQuote(base), "--file", shellQuote(file))
	scopeURL := strings.TrimRight(base, "/")
	if repo != "" {
		parts = append(parts, "--repository-url", shellQuote(repo))
		scopeURL = strings.TrimRight(repo, "/")
	}
	return []string{
		"-c", "credential.useHttpPath=true",
		"-c", "credential." + scopeURL + ".helper=!" + strings.Join(parts, " "),
	}, nil
}
