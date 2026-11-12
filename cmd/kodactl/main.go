// Command kodactl is the thin Userland CLI entrypoint. It starts no server and
// no background process: it parses its own connection flags, constructs the real
// login runtime and delegates every command to the injected Userland CLI command
// logic.
package main

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/app/drivers/sessionlock"
	"github.com/optimiware/kodama/internal/shell/app/drivers/userlandfs"
	"github.com/optimiware/kodama/internal/userland/cli"
)

// defaultSessionLockTimeout bounds one acquisition of the cross-process session
// lock when the caller's context carries no deadline. The lock itself waits
// indefinitely, so every production Acquire is bounded here.
const defaultSessionLockTimeout = 30 * time.Second

// cliDeps are the injectable CLI collaborators. The zero-value LoginRuntime is
// never used: run always constructs the real runtime unless a test injects a
// specific port.
type cliDeps struct {
	runCLI      func(context.Context, cli.Config, []string, cli.Ports, cli.CommandDeps) int
	oidc        cli.OIDCClient
	browser     cli.BrowserOpener
	loopback    cli.LoopbackListener
	sessionLock cli.SessionLock
	clock       cli.Clock
	random      cli.Random
	environment func(string) string
	lockTimeout time.Duration
}

func defaultCLIDeps() cliDeps {
	return cliDeps{runCLI: cli.RunWithDeps}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr, defaultCLIDeps()))
}

// run parses the leading connection flags, constructs the real login runtime
// and delegates every command to the Userland CLI. It never starts a server.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, deps cliDeps) int {
	flags := flag.NewFlagSet("kodactl", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configFile := flags.String("config", envOr("KODAMA_CONFIG", defaultConfigFile()), "kodactl configuration file")
	serverDefault := firstEnv("KODAMA_SERVER", "KODAMA_SERVER_URL")
	serverURL := serverDefault
	flags.StringVar(&serverURL, "server", serverDefault, "public server base URL")
	flags.StringVar(&serverURL, "server-url", serverDefault, "alias for --server")
	credentialFile := flags.String("credential-file", envOr("KODAMA_CREDENTIAL_FILE", ""), "mounted execution credential file")
	output := flags.String("output", envOr("KODAMA_OUTPUT", ""), "output format (table|json)")
	project := flags.String("project", envOr("KODAMA_PROJECT", ""), "project identity")
	sessionDefault := firstEnv("KODAMA_SESSION", "KODAMA_SESSION_FILE")
	if sessionDefault == "" {
		sessionDefault = defaultSessionFile()
	}
	sessionFile := sessionDefault
	flags.StringVar(&sessionFile, "session", sessionDefault, "human session file")
	flags.StringVar(&sessionFile, "session-file", sessionDefault, "alias for --session")
	lockDefault := envDuration("KODAMA_SESSION_LOCK_TIMEOUT", defaultSessionLockTimeout)
	lockTimeout := lockDefault
	flags.DurationVar(&lockTimeout, "session-lock-timeout", lockDefault, "maximum wait for the human-session lock")
	trustedOrigins := stringList{}
	flags.Var(&trustedOrigins, "trusted-origin", "registered trusted Git origin (repeatable)")
	showVersion := flags.Bool("version", false, "print the version and source revision, then exit")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	// The version needs no configuration, session or server.
	if *showVersion || flags.Arg(0) == "version" {
		printVersion(stdout)
		return 0
	}

	config, code := loadConfig(flags, *configFile, serverURL, *credentialFile, *output, *project, trustedOrigins, stderr)
	if code != 0 {
		return code
	}
	if deps.lockTimeout > 0 {
		lockTimeout = deps.lockTimeout
	}

	now := time.Now
	clock := deps.clock
	if clock == nil {
		clock = systemClock{}
	} else {
		now = clock.Now
	}
	files := userlandfs.FileSystem{}
	store := cli.FileSessionStore{Path: sessionFile, Files: files, Now: now}
	oidc := deps.oidc
	if oidc == nil {
		oidc = newHTTPOIDCClient()
	}
	lock := newBoundedSessionLock(sessionLockFor(sessionFile, deps.sessionLock), lockTimeout)
	browser := deps.browser
	if browser == nil {
		browser = systemBrowser{}
	}
	loopback := deps.loopback
	if loopback == nil {
		loopback = cli.NewLoopbackListener()
	}
	random := deps.random
	if random == nil {
		random = cryptorand.Reader
	}
	environment := deps.environment
	if environment == nil {
		environment = os.Getenv
	}
	runtimeDeps := cli.LoginRuntime{
		OIDC:        oidc,
		Browser:     browser,
		Loopback:    loopback,
		Random:      random,
		Clock:       clock,
		Lock:        lock,
		Store:       store,
		Environment: environment,
	}
	source := credentialSource(config.CredentialFile, sessionFile, store, lock, oidc, config.ServerURL, clock, now)
	ports := cli.Ports{
		Files:      files,
		Session:    store,
		Credential: source,
		Out:        stdout,
		Err:        stderr,
		Now:        now,
		Sleep:      time.Sleep,
	}
	if strings.TrimSpace(config.ServerURL) != "" {
		retry := client.RetryPolicy{MaxAttempts: 3, BaseDelay: 100 * time.Millisecond, MaxDelay: time.Second}
		ports.Client = &client.HTTPTransport{
			BaseURL:      config.ServerURL,
			RoundTripper: http.DefaultTransport,
			Source:       source,
			Retry:        retry,
			Now:          now,
		}
	}
	return deps.runCLI(ctx, config, flags.Args(), ports, cli.CommandDeps{Login: runtimeDeps})
}

// credentialSource selects the human session file outside a container and the
// mounted execution credential file inside one. The chosen source is read again
// on every attempt. The human source keeps the owner-only file and directory
// rule in front of the version 2 session logic.
func credentialSource(credentialFile, sessionFile string, store cli.SessionRecordStore, lock cli.SessionLock,
	oidc cli.OIDCClient, apiURL string, clock cli.Clock, now func() time.Time) client.CredentialSource {
	if strings.TrimSpace(credentialFile) != "" {
		return client.FileCredentialSource{Path: credentialFile}
	}
	return sessionCredentialSource{
		path: sessionFile,
		inner: cli.SessionCredentialSource{
			Store:  store,
			Lock:   lock,
			OIDC:   oidc,
			APIURL: apiURL,
			Now:    now,
			Clock:  clock,
		},
	}
}

// sessionCredentialSource enforces the restrictive session file mode and the
// owner-only session directory mode before delegating to the renewing version 2
// session credential source.
type sessionCredentialSource struct {
	inner client.CredentialSource
	path  string
}

var _ client.CredentialSource = sessionCredentialSource{}

// Credential returns the current human session credential.
func (source sessionCredentialSource) Credential(ctx context.Context) (client.Credential, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return client.Credential{}, fault.Wrap(fault.Unauthenticated, "human session read canceled", err)
	}
	if source.inner == nil || strings.TrimSpace(source.path) == "" {
		return client.Credential{}, fault.New(fault.Unauthenticated, "the human session store is not configured")
	}
	if err := checkSessionPermissions(source.path); err != nil {
		return client.Credential{}, err
	}
	return source.inner.Credential(ctx)
}

// checkSessionPermissions enforces the restrictive session file mode and the
// owner-only session directory mode.
func checkSessionPermissions(path string) error {
	fileInfo, err := os.Lstat(path)
	if err != nil {
		return fault.New(fault.Unauthenticated, "the human session file is unreadable")
	}
	if !fileInfo.Mode().IsRegular() {
		return fault.New(fault.Unauthenticated, "the human session file is not a regular file")
	}
	if fileInfo.Mode().Perm() != 0o600 {
		return fault.New(fault.Unauthenticated, "the human session file permissions are not 0600")
	}
	directoryInfo, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return fault.New(fault.Unauthenticated, "the human session directory is unreadable")
	}
	if !directoryInfo.IsDir() {
		return fault.New(fault.Unauthenticated, "the human session directory is not a directory")
	}
	if directoryInfo.Mode().Perm() != 0o700 {
		return fault.New(fault.Unauthenticated, "the human session directory permissions are not 0700")
	}
	return nil
}

// sessionLockFor returns the injected lock or the real cross-process lock bound
// to the session file. A path the lock refuses is reported on every Acquire
// instead of blocking the whole entrypoint.
func sessionLockFor(sessionFile string, injected cli.SessionLock) cli.SessionLock {
	if injected != nil {
		return injected
	}
	lock, err := sessionlock.NewSessionLock(sessionFile)
	if err != nil {
		return unavailableSessionLock{err: err}
	}
	return lock
}

// unavailableSessionLock reports a lock that could not be constructed.
type unavailableSessionLock struct {
	err error
}

var _ cli.SessionLock = unavailableSessionLock{}

// Acquire always refuses with the construction failure.
func (lock unavailableSessionLock) Acquire(context.Context) (func() error, error) {
	if lock.err == nil {
		return nil, errors.New("the session lock is unavailable")
	}
	return nil, lock.err
}

// boundedSessionLock bounds every Acquire that arrives without a deadline. The
// underlying flock has no maximum wait, so an unbounded caller would otherwise
// hang forever behind a live holder. A caller deadline is preserved unchanged.
type boundedSessionLock struct {
	lock    cli.SessionLock
	timeout time.Duration
}

var _ cli.SessionLock = (*boundedSessionLock)(nil)

// newBoundedSessionLock wraps one lock with the default or supplied bound.
func newBoundedSessionLock(lock cli.SessionLock, timeout time.Duration) cli.SessionLock {
	if timeout <= 0 {
		timeout = defaultSessionLockTimeout
	}
	return &boundedSessionLock{lock: lock, timeout: timeout}
}

// Acquire acquires the underlying lock under a deadline-bearing context.
func (bounded *boundedSessionLock) Acquire(ctx context.Context) (func() error, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if bounded == nil || bounded.lock == nil {
		return nil, errors.New("the session lock is not configured")
	}
	timeout := bounded.timeout
	if timeout <= 0 {
		timeout = defaultSessionLockTimeout
	}
	if _, hasDeadline := ctx.Deadline(); hasDeadline {
		return bounded.lock.Acquire(ctx)
	}
	boundedCtx, cancel := context.WithTimeout(ctx, timeout)
	release, err := bounded.lock.Acquire(boundedCtx)
	if err != nil {
		cancel()
		return nil, err
	}
	return func() error {
		defer cancel()
		if release == nil {
			return nil
		}
		return release()
	}, nil
}

// systemClock is the production time and cancellable sleep boundary.
type systemClock struct{}

// Now returns the wall clock.
func (systemClock) Now() time.Time { return time.Now() }

// Sleep waits for duration or the context, whichever comes first.
func (systemClock) Sleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// newHTTPOIDCClient builds the production OAuth/OIDC adapter over a transport
// with default TLS verification and no proxy for loopback endpoints.
func newHTTPOIDCClient() cli.OIDCClient {
	return &cli.HTTPOIDCClient{RoundTripper: newOIDCTransport()}
}

// newOIDCTransport clones the default HTTP transport, keeps certificate
// verification at its default (InsecureSkipVerify is never set) and bypasses any
// configured proxy for a loopback OAuth issuer.
func newOIDCTransport() http.RoundTripper {
	var transport *http.Transport
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = base.Clone()
	} else {
		transport = &http.Transport{}
	}
	transport.Proxy = oidcProxy
	configuration := transport.TLSClientConfig
	if configuration == nil {
		configuration = &tls.Config{}
	} else {
		configuration = configuration.Clone()
	}
	// The adapter never disables certificate verification.
	configuration.InsecureSkipVerify = false
	transport.TLSClientConfig = configuration
	return transport
}

// oidcProxy never proxies a loopback or localhost request.
func oidcProxy(request *http.Request) (*url.URL, error) {
	if request == nil || request.URL == nil {
		return nil, nil
	}
	host := request.URL.Hostname()
	if host == "localhost" {
		return nil, nil
	}
	if address := net.ParseIP(host); address != nil && address.IsLoopback() {
		return nil, nil
	}
	return http.ProxyFromEnvironment(request)
}

// systemBrowser opens one authorization URL with the platform browser launcher.
// It never runs a shell and always passes the URL as exactly one argument.
type systemBrowser struct{}

var _ cli.BrowserOpener = systemBrowser{}

// Open starts the platform launcher without a shell and a non-nil environment.
func (systemBrowser) Open(ctx context.Context, target string) error {
	if err := validateBrowserTarget(target); err != nil {
		return err
	}
	name, argv := browserInvocation(runtime.GOOS, target)
	command := exec.CommandContext(ctx, name, argv...)
	command.Env = browserEnvironment()
	command.Stdout = io.Discard
	var diagnostics bytes.Buffer
	command.Stderr = &diagnostics
	if err := command.Start(); err != nil {
		return err
	}
	return command.Wait()
}

// browserEnvironment returns the explicit, never-nil launcher environment.
func browserEnvironment() []string {
	environment := os.Environ()
	if environment == nil {
		environment = []string{}
	}
	return environment
}

// browserInvocation returns the platform launcher and its argv. The target URL
// is one argv element, never a shell command fragment.
func browserInvocation(goos, target string) (string, []string) {
	switch goos {
	case "darwin":
		return "open", []string{target}
	case "windows":
		return "rundll32", []string{"url.dll,FileProtocolHandler", target}
	default:
		return "xdg-open", []string{target}
	}
}

// validateBrowserTarget refuses anything but an absolute http(s) URL.
func validateBrowserTarget(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.User != nil || parsed.Opaque != "" {
		return errors.New("the browser URL is not an absolute URL")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		return nil
	default:
		return errors.New("the browser URL scheme is not supported")
	}
}

// loadConfig merges the configuration file with the explicit flags and
// environment values. Explicit flags and environment values win over the file.
// A missing default file is not an error; an explicitly requested unreadable
// file and an invalid configuration are usage refusals.
func loadConfig(flags *flag.FlagSet, configFile, serverURL, credentialFile, output, project string, trustedOrigins stringList, stderr io.Writer) (cli.Config, int) {
	explicitConfig := flagSet(flags, "config") || envSet("KODAMA_CONFIG")
	config := cli.Config{}
	if configPath := strings.TrimSpace(configFile); configPath != "" {
		data, err := os.ReadFile(configPath)
		switch {
		case err == nil:
			loaded, parseErr := cli.ParseConfig(data)
			if parseErr != nil {
				return cli.Config{}, usage(stderr, parseErr.Error())
			}
			config = loaded
		case errors.Is(err, fs.ErrNotExist) && !explicitConfig:
			// No default configuration file is a normal workstation state.
		default:
			return cli.Config{}, usage(stderr, fmt.Sprintf("kodactl config %q is unreadable", configPath))
		}
	}
	explicitServer := flagSet(flags, "server") || flagSet(flags, "server-url") ||
		envSet("KODAMA_SERVER") || envSet("KODAMA_SERVER_URL")
	if explicitServer {
		config.ServerURL = serverURL
	}
	if flagSet(flags, "credential-file") || envSet("KODAMA_CREDENTIAL_FILE") {
		config.CredentialFile = credentialFile
	}
	if flagSet(flags, "output") || envSet("KODAMA_OUTPUT") {
		config.Output = cli.OutputFormat(output)
	}
	if flagSet(flags, "project") || envSet("KODAMA_PROJECT") {
		config.Project = project
	}
	if config.Output == "" {
		config.Output = cli.OutputTable
	}
	if len(trustedOrigins) > 0 {
		config.TrustedOrigins = append(append([]string(nil), config.TrustedOrigins...), trustedOrigins...)
	}
	return config, 0
}

// stringList is a repeatable flag value.
type stringList []string

// String renders the collected values.
func (list *stringList) String() string { return strings.Join(*list, ",") }

// Set appends one value.
func (list *stringList) Set(value string) error {
	*list = append(*list, value)
	return nil
}

// usage writes one usage diagnostic and returns the usage exit code.
func usage(stderr io.Writer, message string) int {
	fmt.Fprintf(stderr, "usage error: %s\n", message)
	return 2
}

// flagSet reports whether one flag was set on the command line.
func flagSet(flags *flag.FlagSet, name string) bool {
	found := false
	flags.Visit(func(candidate *flag.Flag) {
		if candidate.Name == name {
			found = true
		}
	})
	return found
}

// envSet reports whether one environment variable is present.
func envSet(name string) bool {
	_, ok := os.LookupEnv(name)
	return ok
}

// envOr reads one environment value or returns its fallback.
func envOr(name, fallback string) string {
	if value, ok := os.LookupEnv(name); ok {
		return value
	}
	return fallback
}

// firstEnv returns the first present environment value.
func firstEnv(names ...string) string {
	for _, name := range names {
		if value, ok := os.LookupEnv(name); ok {
			return value
		}
	}
	return ""
}

// envDuration reads one duration environment value or returns its fallback.
func envDuration(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}

// defaultSessionFile returns the human-session path under the user config
// directory.
func defaultSessionFile() string {
	directory, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(directory, "kodama", "session.json")
}

// defaultConfigFile returns the kodactl configuration path under the user
// config directory.
func defaultConfigFile() string {
	directory, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(directory, "kodama", "config.json")
}
