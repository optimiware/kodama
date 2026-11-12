package messaging

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/test/support/infrastructure/harness"
	runtimesupport "github.com/optimiware/kodama/test/support/infrastructure/runtime"
)

// The deterministic fixture build is the foundation the recipe-image tests
// rely on: the same immutable base and the same fixture bytes must reduce to
// one normalized build input, no matter which filesystem created the bytes,
// which order the files were enumerated in, or what mtimes the source files
// carried. The bit-for-bit build proof runs against real Podman in the
// component package; this test pins the normalization rules that make that
// proof meaningful.

const fixtureTestBaseDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// fixturePinnedDigest freezes the manifest digest the Git-capable fixture base
// was resolved to. Updating the base tag or its digest is a deliberate change
// that must update this pin and the Containerfile together; the real component
// suite also pulls the tag once and fails when the registry serves a different
// digest.
const fixturePinnedDigest = "sha256:712f969c9d81a35f9f00cce701faae521891d42a20b592b61bd0e6c5328fad7c"

// TestFixtureBasePinIsTheGitCapableContainerfileBase freezes the one base the
// fixture may build from and proves the committed Containerfile names exactly
// that digest-pinned reference as its only FROM. The factory needs a Git
// binary, so the base is the SCM buildpack image; a changed base tag, a changed
// digest, a mutable tag or a second FROM must fail here before any real build
// can silently use a different image.
func TestFixtureBasePinIsTheGitCapableContainerfileBase(t *testing.T) {
	const wantTag = "docker.io/library/buildpack-deps:trixie-scm"
	if FixtureBaseTag != wantTag {
		t.Fatalf("FixtureBaseTag = %q; want the pinned Git-capable base %q", FixtureBaseTag, wantTag)
	}
	if FixtureBaseDigest != fixturePinnedDigest {
		t.Fatalf("FixtureBaseDigest = %q; want the resolved pin %q", FixtureBaseDigest, fixturePinnedDigest)
	}
	want, err := NewFixtureBase(FixtureBaseTag, FixtureBaseDigest)
	if err != nil {
		t.Fatalf("build the pinned base reference: %v", err)
	}

	// The embedded Containerfile is the authoritative base declaration: exactly
	// one FROM, written as the digest-pinned tag reference and nothing else.
	var froms []string
	for line := range strings.SplitSeq(string(fixtureContainerfile), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) > 0 && strings.EqualFold(fields[0], "FROM") {
			froms = append(froms, strings.Join(fields[1:], " "))
		}
	}
	if len(froms) != 1 {
		t.Fatalf("Containerfile declares %d FROM instructions; want exactly the one digest-pinned base: %v", len(froms), froms)
	}
	if wantLine := FixtureBaseTag + "@" + FixtureBaseDigest; froms[0] != wantLine {
		t.Fatalf("Containerfile FROM = %q; want the pinned %q", froms[0], wantLine)
	}

	// PinnedFixtureBase is the value the builder uses; it must normalize the
	// Containerfile reference to the same immutable repository@digest identity.
	pinned, err := PinnedFixtureBase()
	if err != nil {
		t.Fatalf("resolve the pinned fixture base: %v", err)
	}
	if pinned.Reference != want.Reference {
		t.Fatalf("PinnedFixtureBase reference = %q; want %q", pinned.Reference, want.Reference)
	}
	if !strings.Contains(pinned.Reference, "@"+FixtureBaseDigest) {
		t.Fatalf("PinnedFixtureBase reference %q does not pin the digest %q", pinned.Reference, FixtureBaseDigest)
	}

	// The same helper Build uses must refuse a Containerfile whose single base
	// moved to another repository or another digest: the drift check is not a
	// property of only the committed bytes.
	for _, containerfile := range []string{
		"FROM docker.io/library/alpine@" + FixtureBaseDigest + "\n",
		"FROM " + FixtureBaseTag + "@" + fixtureTestBaseDigest + "\n",
	} {
		if _, err := pinnedFixtureBase([]byte(containerfile)); err == nil {
			t.Fatalf("pinned fixture base accepted drifted Containerfile %q", containerfile)
		}
	}
}

// TestParseFixtureBaseRefusesMutableOrMultipleBases pins the parser's refusal
// cases. A mutable tag cannot name the image the build uses, a build argument
// makes the Containerfile agree with anything the caller supplies, a second
// FROM adds an unowned base, and a FROM flag changes the resolved platform
// without changing the pinned reference.
func TestParseFixtureBaseRefusesMutableOrMultipleBases(t *testing.T) {
	valid, err := parseFixtureBase([]byte("# comment FROM is ignored\r\nFROM " + FixtureBaseTag + "@" + FixtureBaseDigest + "\r\n"))
	if err != nil {
		t.Fatalf("parse the pinned Containerfile: %v", err)
	}
	want, err := NewFixtureBase(FixtureBaseTag, FixtureBaseDigest)
	if err != nil {
		t.Fatalf("build the pinned base reference: %v", err)
	}
	if valid.Reference != want.Reference {
		t.Fatalf("parsed reference = %q; want the normalized %q", valid.Reference, want.Reference)
	}

	refused := []struct {
		name          string
		containerfile string
	}{
		{"mutable tag", "FROM " + FixtureBaseTag + "\n"},
		{"build argument", "ARG FIXTURE_BASE\nFROM ${FIXTURE_BASE}\n"},
		{"multiple bases", "FROM " + FixtureBaseTag + "@" + FixtureBaseDigest + "\nFROM docker.io/library/alpine@" + FixtureBaseDigest + "\n"},
		{"platform flag", "FROM --platform=linux/amd64 " + FixtureBaseTag + "@" + FixtureBaseDigest + "\n"},
		{"no base", "RUN true\n"},
	}
	for _, test := range refused {
		if _, err := parseFixtureBase([]byte(test.containerfile)); err == nil {
			t.Fatalf("%s: Containerfile %q was accepted; want a refusal", test.name, test.containerfile)
		}
	}
}

// TestFixtureBuildRefusesBaseOutsideContainerfilePin pins the build-time drift
// check: the caller resolves the tag once, but Build refuses a resolution that
// disagrees with the committed Containerfile pin before it runs Podman at all.
func TestFixtureBuildRefusesBaseOutsideContainerfilePin(t *testing.T) {
	runner := &recordingRunner{}
	builder := NewFixtureBuilder(WithFixtureRunner(runner), WithFixtureEnv([]string{"PATH=/bin"}))
	other, err := NewFixtureBase(FixtureBaseTag, fixtureTestBaseDigest)
	if err != nil {
		t.Fatalf("build the mismatched base reference: %v", err)
	}
	_, err = builder.Build(context.Background(), other, FixtureInputs{Marker: "fixture-drift\n"}, "localhost/kodama-fixture-unit:drift")
	if err == nil {
		t.Fatal("build accepted a base that differs from the Containerfile pin")
	}
	if !strings.Contains(err.Error(), "Containerfile") {
		t.Fatalf("base mismatch error %q does not name the Containerfile pin", err)
	}
	if commands := runner.recorded(); len(commands) != 0 {
		t.Fatalf("build issued commands before refusing the mismatched base: %v", commands)
	}
}

type fixtureSourceEntry struct {
	path    string
	content string
}

// writeFixtureSource creates one source directory whose files are written in
// the supplied order with distinct mtimes, then reads them back through
// ReadFixtureFiles. Reading normalizes the representation to bytes, paths and
// modes, so two semantically equal directories may be compared without
// depending on how the filesystem enumerated them or when it stamped them.
func writeFixtureSource(t *testing.T, entries []fixtureSourceEntry, mtime time.Time) []FixtureFile {
	t.Helper()
	dir := t.TempDir()
	for _, entry := range entries {
		path := filepath.Join(dir, entry.path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create source directory: %v", err)
		}
		if err := os.WriteFile(path, []byte(entry.content), 0o644); err != nil {
			t.Fatalf("write source file %s: %v", entry.path, err)
		}
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatalf("stamp source file %s: %v", entry.path, err)
		}
	}
	files, err := ReadFixtureFiles(dir)
	if err != nil {
		t.Fatalf("read fixture source directory: %v", err)
	}
	return files
}

func TestFixtureBuildInputsAreDeterministic(t *testing.T) {
	baseReference := "docker.io/library/buildpack-deps@" + fixtureTestBaseDigest

	// Two sources with identical bytes, different enumeration order and
	// different ambient mtimes.
	first := writeFixtureSource(t, []fixtureSourceEntry{
		{path: "z.txt", content: "last\n"},
		{path: "a.txt", content: "first\n"},
	}, time.Unix(1_700_000_000, 0))
	second := writeFixtureSource(t, []fixtureSourceEntry{
		{path: "a.txt", content: "first\n"},
		{path: "z.txt", content: "last\n"},
	}, time.Unix(1_234_567_890, 0))
	// ReadFixtureFiles sorts its result; reverse it so the two callers present
	// the same files in opposite orders and normalization must sort them.
	for left, right := 0, len(second)-1; left < right; left, right = left+1, right-1 {
		second[left], second[right] = second[right], second[left]
	}

	spec := FixtureInputs{
		BaseReference: baseReference,
		Marker:        "fixture-alpha\n",
		ExtraFiles:    first,
	}
	inputs, err := NormalizeBuildInputs(spec)
	if err != nil {
		t.Fatalf("normalize first inputs: %v", err)
	}
	reordered := spec
	reordered.ExtraFiles = second
	other, err := NormalizeBuildInputs(reordered)
	if err != nil {
		t.Fatalf("normalize reordered inputs: %v", err)
	}
	if inputs.Descriptor != other.Descriptor {
		t.Fatalf("descriptor changed with source order/mtime: %s != %s", inputs.Descriptor, other.Descriptor)
	}
	if other.BaseReference != baseReference {
		t.Fatalf("normalized base = %q; want %q", other.BaseReference, baseReference)
	}
	if got, want := len(inputs.Files), 2; got != want {
		t.Fatalf("normalized extra files = %d; want %d", got, want)
	}
	if inputs.Files[0].Path != "a.txt" || inputs.Files[1].Path != "z.txt" {
		t.Fatalf("normalized files are not sorted by path: %+v", inputs.Files)
	}

	// Materialization owns every timestamp in the build context: the ambient
	// source mtimes above must not survive, and two materializations of the
	// same normalized input must be indistinguishable.
	contextOne, contextTwo := t.TempDir(), t.TempDir()
	if err := inputs.WriteContext(contextOne); err != nil {
		t.Fatalf("materialize first context: %v", err)
	}
	if err := other.WriteContext(contextTwo); err != nil {
		t.Fatalf("materialize second context: %v", err)
	}
	epoch := time.Unix(0, 0)
	entries, err := os.ReadDir(contextOne)
	if err != nil {
		t.Fatalf("read materialized context: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("materialized context is empty")
	}
	for _, entry := range entries {
		info, statErr := entry.Info()
		if statErr != nil {
			t.Fatalf("stat materialized %s: %v", entry.Name(), statErr)
		}
		if !info.ModTime().Equal(epoch) {
			t.Fatalf("materialized %s mtime = %s; want the fixed build epoch %s", entry.Name(), info.ModTime(), epoch)
		}
	}
	firstFingerprint, err := FingerprintBuildContext(contextOne)
	if err != nil {
		t.Fatalf("fingerprint first context: %v", err)
	}
	secondFingerprint, err := FingerprintBuildContext(contextTwo)
	if err != nil {
		t.Fatalf("fingerprint second context: %v", err)
	}
	if firstFingerprint != secondFingerprint {
		t.Fatalf("materialized contexts differ: %s != %s", firstFingerprint, secondFingerprint)
	}

	// A mutable base tag is refused: the base must already be the immutable
	// digest the caller resolved once.
	if _, err := NormalizeBuildInputs(FixtureInputs{
		BaseReference: FixtureBaseTag,
		Marker:        "fixture-alpha\n",
	}); err == nil {
		t.Fatal("mutable base tag was accepted; want an immutable digest requirement")
	}
	// A tag carried beside a digest is not a second identity: it normalizes to
	// the same immutable repository@digest reference.
	tagged, err := NormalizeBaseReference(FixtureBaseTag + "@" + fixtureTestBaseDigest)
	if err != nil {
		t.Fatalf("normalize tag@digest base: %v", err)
	}
	if tagged != baseReference {
		t.Fatalf("tag@digest base = %q; want %q", tagged, baseReference)
	}
	if _, err := NormalizeBaseReference("docker.io/library/buildpack-deps@" + fixtureTestBaseDigest + "x"); err == nil {
		t.Fatal("malformed digest was accepted")
	}

	// Fixture content is part of the identity: changing the marker must change
	// the normalized inputs, so a build that ignored it cannot look stable.
	changed := spec
	changed.Marker = "fixture-beta\n"
	changedInputs, err := NormalizeBuildInputs(changed)
	if err != nil {
		t.Fatalf("normalize changed marker: %v", err)
	}
	if changedInputs.Descriptor == inputs.Descriptor {
		t.Fatal("descriptor ignored the marker content")
	}
}

// TestFixtureBuildArgsPinReproducibilityControls keeps the build controls that
// are not observable from the normalized input descriptor pinned: no image
// pull during the build, no network (so no package installation), no cache, a
// fixed timestamp and OCI output. The base is not a build argument:
// the Containerfile's single FROM is the only base, so no caller-supplied
// build argument may override it. A build that merely happens to be repeatable
// on one host is not enough.
func TestFixtureBuildArgsPinReproducibilityControls(t *testing.T) {
	args := fixtureBuildArgs("localhost/kodama-fixture-example:first", "/tmp/context")
	joined := strings.Join(args, " ")
	for _, required := range []string{
		"build",
		"--pull=never",
		"--network=none",
		"--no-cache",
		"--timestamp 0",
		"--format oci",
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("build arguments %q are missing %q", joined, required)
		}
	}
	if strings.Contains(joined, "--build-arg") {
		t.Fatalf("build arguments %q still override the Containerfile base with a build argument", joined)
	}

	// SOURCE_DATE_EPOCH is a builder-owned control: a caller-supplied value is
	// replaced, not merged, and exactly one value is present.
	builder := NewFixtureBuilder(WithFixtureEnv([]string{"PATH=/bin", "SOURCE_DATE_EPOCH=1700000000"}))
	epochs := 0
	for _, value := range builder.env {
		if strings.HasPrefix(value, "SOURCE_DATE_EPOCH=") {
			epochs++
			if value != "SOURCE_DATE_EPOCH=0" {
				t.Fatalf("fixture build env carries %q; want the fixed SOURCE_DATE_EPOCH=0", value)
			}
		}
	}
	if epochs != 1 {
		t.Fatalf("fixture build env has %d SOURCE_DATE_EPOCH values; want exactly 1", epochs)
	}
}

// recordingRunner captures the podman argument vectors the fixture builder
// issues, so cleanup policy is observable without a container runtime. The
// optional handler scripts command outcomes; without one every command
// succeeds.
type recordingRunner struct {
	mu       sync.Mutex
	commands [][]string
	handler  func(harness.Command) (harness.Result, error)
}

func (runner *recordingRunner) Run(_ context.Context, command harness.Command) (harness.Result, error) {
	runner.mu.Lock()
	runner.commands = append(runner.commands, append([]string(nil), command.Args...))
	handler := runner.handler
	runner.mu.Unlock()
	if handler != nil {
		return handler(command)
	}
	return harness.Result{}, nil
}

// recorded returns a snapshot of the argument vectors issued so far.
func (runner *recordingRunner) recorded() [][]string {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	out := make([][]string, 0, len(runner.commands))
	for _, command := range runner.commands {
		out = append(out, append([]string(nil), command...))
	}
	return out
}

func commandArg(args []string, flag string) string {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == flag {
			return args[index+1]
		}
	}
	return ""
}

// fixtureManifestServer serves exactly the manifest bodies the fake push
// recorded, so Publish's registry validation runs against a real HTTP
// response rather than a local image ID.
func fixtureManifestServer(t *testing.T, repository string, bodies map[string][]byte) *Registry {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		prefix := "/v2/" + repository + "/manifests/"
		if !strings.HasPrefix(request.URL.Path, prefix) {
			http.NotFound(writer, request)
			return
		}
		digest := strings.TrimPrefix(request.URL.Path, prefix)
		body, ok := bodies[digest]
		if !ok {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Content-Type", OCIManifestMediaType)
		writer.Header().Set("Docker-Content-Digest", digest)
		_, _ = writer.Write(body)
	}))
	t.Cleanup(server.Close)
	return &Registry{BaseURL: server.URL}
}

// TestFixtureCleanupRemovesEveryCreatedName drives the full build, publish and
// run flow against the fake runner. Every name this fixture creates or pulls -
// the local build tag, each registry-qualified push tag (including the
// retargeted tag) and each digest-pinned pull name - must be removed by exact
// name, and nothing else. Removal is never forced, never by image ID and never
// a prune, so shared content-addressed cache entries and a twin's names
// survive.
func TestFixtureCleanupRemovesEveryCreatedName(t *testing.T) {
	repository := "kodama/fixture-unit-retarget"
	bodyA := []byte(`{"schemaVersion":2,"content":"fixture-A"}`)
	bodyB := []byte(`{"schemaVersion":2,"content":"fixture-B"}`)
	digestA := DigestOf(bodyA)
	digestB := DigestOf(bodyB)
	instance := fixtureManifestServer(t, repository, map[string][]byte{digestA: bodyA, digestB: bodyB})

	const imageID = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	runner := &recordingRunner{}
	runner.handler = func(command harness.Command) (harness.Result, error) {
		args := command.Args
		switch {
		case len(args) == 1 && args[0] == "--version":
			return harness.Result{Stdout: []byte("podman version 5.4.2\n")}, nil
		case args[0] == "build":
			return harness.Result{}, nil
		case args[0] == "image":
			return harness.Result{Stdout: []byte(imageID + "\n")}, nil
		case args[0] == "push":
			source := args[len(args)-2]
			digest := digestA
			if strings.HasSuffix(source, ":second") {
				digest = digestB
			}
			if err := os.WriteFile(commandArg(args, "--digestfile"), []byte(digest+"\n"), 0o600); err != nil {
				return harness.Result{}, err
			}
			return harness.Result{}, nil
		case args[0] == "run":
			return harness.Result{Stdout: []byte("fixture-A\n")}, nil
		}
		return harness.Result{}, nil
	}
	builder := NewFixtureBuilder(WithFixtureRunner(runner), WithFixtureEnv([]string{"PATH=/bin"}))
	base, err := PinnedFixtureBase()
	if err != nil {
		t.Fatalf("resolve the pinned fixture base: %v", err)
	}
	ctx := context.Background()
	localFirst := "localhost/kodama-fixture-unit:first"
	localSecond := "localhost/kodama-fixture-unit:second"

	first, err := builder.Build(ctx, base, FixtureInputs{Marker: "fixture-A\n"}, localFirst)
	if err != nil {
		t.Fatalf("build fixture first: %v", err)
	}
	publishedFirst, err := builder.Publish(ctx, instance, first, repository, "current")
	if err != nil {
		t.Fatalf("publish fixture first: %v", err)
	}
	if _, err := builder.RunMarker(ctx, instance, publishedFirst.RunnableReference); err != nil {
		t.Fatalf("run fixture first by digest: %v", err)
	}
	second, err := builder.Build(ctx, base, FixtureInputs{Marker: "fixture-B\n"}, localSecond)
	if err != nil {
		t.Fatalf("build fixture second: %v", err)
	}
	publishedSecond, err := builder.Publish(ctx, instance, second, repository, "current")
	if err != nil {
		t.Fatalf("publish fixture second over the retargeted tag: %v", err)
	}
	retargetedTag := instance.Host() + "/" + repository + ":current"
	if _, err := builder.RunMarker(ctx, instance, retargetedTag); err != nil {
		t.Fatalf("run retargeted tag: %v", err)
	}
	if _, err := builder.RunMarker(ctx, instance, publishedSecond.RunnableReference); err != nil {
		t.Fatalf("run fixture second by digest: %v", err)
	}

	before := len(runner.recorded())
	if err := builder.Cleanup(ctx); err != nil {
		t.Fatalf("cleanup fixture names: %v", err)
	}
	cleanupCommands := runner.recorded()[before:]
	want := [][]string{
		{"rmi", localFirst},
		{"rmi", retargetedTag},
		{"rmi", publishedFirst.RunnableReference},
		{"rmi", localSecond},
		{"rmi", publishedSecond.RunnableReference},
	}
	if len(cleanupCommands) != len(want) {
		t.Fatalf("cleanup issued %d commands; want exactly %d: %v", len(cleanupCommands), len(want), cleanupCommands)
	}
	for index, command := range cleanupCommands {
		if strings.Join(command, " ") != strings.Join(want[index], " ") {
			t.Fatalf("cleanup command %d = %q; want %q", index, strings.Join(command, " "), strings.Join(want[index], " "))
		}
		if len(command) != 2 || command[0] != "rmi" {
			t.Fatalf("cleanup command %q is not an unforced rmi by exact name", strings.Join(command, " "))
		}
		if command[1] == imageID || strings.EqualFold(command[1], strings.TrimPrefix(imageID, "sha256:")) {
			t.Fatalf("cleanup removed an image by ID: %q", command[1])
		}
	}
	for _, command := range runner.recorded() {
		if len(command) > 0 && command[0] == "prune" {
			t.Fatalf("cleanup issued a prune command: %v", command)
		}
		for _, arg := range command {
			if arg == "--force" || strings.HasPrefix(arg, "--force=") {
				t.Fatalf("cleanup forced removal: %v", command)
			}
		}
	}

	// Cleanup is idempotent: every successful or already-gone name is
	// forgotten, so a second call issues no commands and reports no error.
	settled := len(runner.recorded())
	if err := builder.Cleanup(ctx); err != nil {
		t.Fatalf("second cleanup: %v", err)
	}
	if retried := runner.recorded()[settled:]; len(retried) != 0 {
		t.Fatalf("second cleanup issued commands for already-removed names: %v", retried)
	}
}

// TestFixtureCleanupReportsRemovalFailures keeps hard removal failures visible:
// a name that could not be removed is reported, the other names are still
// attempted, and only the failed name is retried later. "image not known" is
// the one tolerated outcome because another actor may already have removed the
// name.
func TestFixtureCleanupReportsRemovalFailures(t *testing.T) {
	localFirst := "localhost/kodama-fixture-unit:first"
	localSecond := "localhost/kodama-fixture-unit:second"
	runner := &recordingRunner{}
	runner.handler = func(command harness.Command) (harness.Result, error) {
		if len(command.Args) > 1 && command.Args[0] == "rmi" && command.Args[1] == localFirst {
			return harness.Result{Stderr: []byte("Error: storage is read-only\n"), ExitCode: 1},
				errors.New("harness: podman rmi failed: exit status 1")
		}
		return harness.Result{}, nil
	}
	builder := NewFixtureBuilder(WithFixtureRunner(runner), WithFixtureEnv([]string{"PATH=/bin"}))
	builder.trackCreated(localFirst, localSecond)

	err := builder.Cleanup(context.Background())
	if err == nil {
		t.Fatal("cleanup swallowed a hard rmi failure")
	}
	if !strings.Contains(err.Error(), localFirst) {
		t.Fatalf("cleanup error %q does not name the failed removal %q", err, localFirst)
	}
	firstAttempt := runner.recorded()
	if len(firstAttempt) != 2 {
		t.Fatalf("cleanup attempted %d removals; want both names: %v", len(firstAttempt), firstAttempt)
	}

	// Only the failed name is still tracked, so a retry does not re-issue the
	// successful removal.
	runner.handler = nil
	before := len(runner.recorded())
	if err := builder.Cleanup(context.Background()); err != nil {
		t.Fatalf("cleanup retry: %v", err)
	}
	retried := runner.recorded()[before:]
	if len(retried) != 1 || strings.Join(retried[0], " ") != "rmi "+localFirst {
		t.Fatalf("cleanup retry = %v; want only the failed %q", retried, localFirst)
	}
}

// TestFixtureCleanupToleratesAlreadyGoneNames pins the one accepted removal
// failure: "image not known" (and "no such image") means the exact name is
// already gone, so cleanup succeeds and does not retry it.
func TestFixtureCleanupToleratesAlreadyGoneNames(t *testing.T) {
	localTag := "localhost/kodama-fixture-unit:first"
	runner := &recordingRunner{}
	runner.handler = func(command harness.Command) (harness.Result, error) {
		if len(command.Args) > 1 && command.Args[0] == "rmi" {
			return harness.Result{Stderr: []byte("Error: image not known\n"), ExitCode: 1},
				errors.New("harness: podman rmi failed: exit status 1")
		}
		return harness.Result{}, nil
	}
	builder := NewFixtureBuilder(WithFixtureRunner(runner), WithFixtureEnv([]string{"PATH=/bin"}))
	builder.trackCreated(localTag)

	if err := builder.Cleanup(context.Background()); err != nil {
		t.Fatalf("cleanup treated an already-gone name as a failure: %v", err)
	}
	settled := len(runner.recorded())
	if err := builder.Cleanup(context.Background()); err != nil {
		t.Fatalf("second cleanup: %v", err)
	}
	if retried := runner.recorded()[settled:]; len(retried) != 0 {
		t.Fatalf("cleanup retried an already-gone name: %v", retried)
	}
}

// assertCleanupRemovesExactly runs Cleanup and requires exactly one unforced
// rmi by exact name for each supplied name, in order.
func assertCleanupRemovesExactly(t *testing.T, builder *FixtureBuilder, runner *recordingRunner, names ...string) {
	t.Helper()
	before := len(runner.recorded())
	if err := builder.Cleanup(context.Background()); err != nil {
		t.Fatalf("cleanup after a failed mutation: %v", err)
	}
	issued := runner.recorded()[before:]
	want := make([][]string, 0, len(names))
	for _, name := range names {
		want = append(want, []string{"rmi", name})
	}
	if len(issued) != len(want) {
		t.Fatalf("cleanup issued %d commands; want %d: %v", len(issued), len(want), issued)
	}
	for index := range want {
		if got, expected := strings.Join(issued[index], " "), strings.Join(want[index], " "); got != expected {
			t.Fatalf("cleanup command %d = %q; want %q", index, got, expected)
		}
	}
}

// TestFixtureCleanupRemovesTagFromFailedBuild pins that the local build tag is
// recorded before the build command can create it: a build that fails must
// still be cleaned up by exact name.
func TestFixtureCleanupRemovesTagFromFailedBuild(t *testing.T) {
	const localTag = "localhost/kodama-fixture-failed:build"
	runner := &recordingRunner{}
	runner.handler = func(command harness.Command) (harness.Result, error) {
		if len(command.Args) == 1 && command.Args[0] == "--version" {
			return harness.Result{Stdout: []byte("podman version 5.4.2\n")}, nil
		}
		if len(command.Args) > 0 && command.Args[0] == "build" {
			return harness.Result{Stderr: []byte("Error: build exploded\n"), ExitCode: 1},
				errors.New("harness: podman build failed: exit status 1")
		}
		return harness.Result{}, nil
	}
	builder := NewFixtureBuilder(WithFixtureRunner(runner), WithFixtureEnv([]string{"PATH=/bin"}))
	base, err := PinnedFixtureBase()
	if err != nil {
		t.Fatalf("resolve the pinned fixture base: %v", err)
	}
	if _, err := builder.Build(context.Background(), base, FixtureInputs{Marker: "fixture\n"}, localTag); err == nil {
		t.Fatal("failed build reported success")
	}
	assertCleanupRemovesExactly(t, builder, runner, localTag)
}

// TestFixtureCleanupRemovesRetargetTagFromFailedPush pins that the
// registry-qualified push tag (including a retargeted tag) is recorded before
// the push command runs: a failed push must still be cleaned up by exact name.
func TestFixtureCleanupRemovesRetargetTagFromFailedPush(t *testing.T) {
	instance := &Registry{BaseURL: "http://127.0.0.1:5000"}
	const repository = "kodama/fixture-failed"
	reference := instance.Host() + "/" + repository + ":current"
	runner := &recordingRunner{}
	runner.handler = func(command harness.Command) (harness.Result, error) {
		if len(command.Args) > 0 && command.Args[0] == "push" {
			return harness.Result{Stderr: []byte("Error: push exploded\n"), ExitCode: 1},
				errors.New("harness: podman push failed: exit status 1")
		}
		return harness.Result{}, nil
	}
	builder := NewFixtureBuilder(WithFixtureRunner(runner), WithFixtureEnv([]string{"PATH=/bin"}))
	image := FixtureImage{Tag: "localhost/kodama-fixture-failed:image", ImageID: fixtureTestBaseDigest}
	if _, err := builder.Publish(context.Background(), instance, image, repository, "current"); err == nil {
		t.Fatal("failed push reported success")
	}
	assertCleanupRemovesExactly(t, builder, runner, reference)
}

// TestFixtureCleanupRemovesReferenceFromFailedRun pins that the reference
// materialized by --pull=always is recorded before the run command runs: a run
// that fails must still be cleaned up by exact name.
func TestFixtureCleanupRemovesReferenceFromFailedRun(t *testing.T) {
	instance := &Registry{BaseURL: "http://127.0.0.1:5000"}
	reference := instance.Host() + "/kodama/fixture-failed@" + fixtureTestBaseDigest
	runner := &recordingRunner{}
	runner.handler = func(command harness.Command) (harness.Result, error) {
		if len(command.Args) > 0 && command.Args[0] == "run" {
			return harness.Result{Stderr: []byte("Error: run exploded\n"), ExitCode: 1},
				errors.New("harness: podman run failed: exit status 1")
		}
		return harness.Result{}, nil
	}
	builder := NewFixtureBuilder(WithFixtureRunner(runner), WithFixtureEnv([]string{"PATH=/bin"}))
	if _, err := builder.RunMarker(context.Background(), instance, reference); err == nil {
		t.Fatal("failed run reported success")
	}
	assertCleanupRemovesExactly(t, builder, runner, reference)
}

// fixtureStubManifestServer always serves one manifest body and one
// Docker-Content-Digest header for the supplied repository, so a test can make
// the bytes and the header lie independently.
func fixtureStubManifestServer(t *testing.T, repository string, body []byte, header string) *Registry {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		prefix := "/v2/" + repository + "/manifests/"
		if !strings.HasPrefix(request.URL.Path, prefix) {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Content-Type", OCIManifestMediaType)
		if header != "" {
			writer.Header().Set("Docker-Content-Digest", header)
		}
		_, _ = writer.Write(body)
	}))
	t.Cleanup(server.Close)
	return &Registry{BaseURL: server.URL}
}

// TestRegistryManifestDigestRejectsMismatchedBytes pins that the manifest body
// itself must hash to the requested digest. The stub serves no
// Docker-Content-Digest header, so a missing bytes check cannot be masked by
// the header check: only comparing the requested digest with the returned
// bytes can reject this body.
func TestRegistryManifestDigestRejectsMismatchedBytes(t *testing.T) {
	const repository = "kodama/fixture-tampered"
	requested := DigestOf([]byte(`{"schemaVersion":2,"content":"honest"}`))
	body := []byte(`{"schemaVersion":2,"content":"tampered"}`)
	instance := fixtureStubManifestServer(t, repository, body, "")
	_, err := RegistryManifestDigest(context.Background(), instance, repository, requested)
	if err == nil {
		t.Fatalf("registry manifest bytes hashing to %q were accepted as %q", DigestOf(body), requested)
	}
	if !strings.Contains(err.Error(), "hash to") {
		t.Fatalf("tampered manifest bytes were rejected for a reason other than the digest mismatch: %v", err)
	}
}

// TestRegistryManifestDigestRejectsLyingHeader pins that a Docker-Content-Digest
// header may not stand in for the returned bytes: honest bytes with a lying
// header must fail even when the reference is a mutable tag.
func TestRegistryManifestDigestRejectsLyingHeader(t *testing.T) {
	const repository = "kodama/fixture-lying-header"
	body := []byte(`{"schemaVersion":2,"content":"honest"}`)
	honest := DigestOf(body)
	lie := DigestOf([]byte(`{"schemaVersion":2,"content":"other"}`))
	instance := fixtureStubManifestServer(t, repository, body, lie)
	if _, err := RegistryManifestDigest(context.Background(), instance, repository, "current"); err == nil {
		t.Fatalf("lying Docker-Content-Digest %q was accepted for honest bytes %q", lie, honest)
	}
}

// newFixturePushRunner fakes a successful push that reports the supplied
// manifest digest, so Publish proceeds to registry validation.
func newFixturePushRunner(digest string) *recordingRunner {
	runner := &recordingRunner{}
	runner.handler = func(command harness.Command) (harness.Result, error) {
		if len(command.Args) > 0 && command.Args[0] == "push" {
			if err := os.WriteFile(commandArg(command.Args, "--digestfile"), []byte(digest+"\n"), 0o600); err != nil {
				return harness.Result{}, err
			}
			return harness.Result{}, nil
		}
		return harness.Result{}, nil
	}
	return runner
}

// TestPublishRejectsTamperedRegistryManifest pins that Publish validates the
// pushed digest against the registry's own bytes, not against whatever digest
// the push reported. The stub serves no Docker-Content-Digest header, so the
// returned bytes must be compared with the pushed digest on their own.
func TestPublishRejectsTamperedRegistryManifest(t *testing.T) {
	const repository = "kodama/fixture-publish-tampered"
	pushed := DigestOf([]byte(`{"schemaVersion":2,"content":"pushed"}`))
	body := []byte(`{"schemaVersion":2,"content":"tampered"}`)
	instance := fixtureStubManifestServer(t, repository, body, "")
	builder := NewFixtureBuilder(WithFixtureRunner(newFixturePushRunner(pushed)), WithFixtureEnv([]string{"PATH=/bin"}))
	image := FixtureImage{Tag: "localhost/kodama-fixture-publish:first", ImageID: fixtureTestBaseDigest}
	_, err := builder.Publish(context.Background(), instance, image, repository, "current")
	if err == nil {
		t.Fatal("publish accepted registry manifest bytes that do not hash to the pushed digest")
	}
	// Either validation layer may report the mismatch: the bytes check in
	// RegistryManifestDigest or Publish's own digest comparison. A transport
	// failure is not a digest mismatch and must not satisfy this test.
	message := err.Error()
	if !strings.Contains(message, "hash to") && !strings.Contains(message, "does not match") {
		t.Fatalf("publish rejected tampered manifest bytes for a reason other than the digest mismatch: %v", err)
	}
}

// TestPublishRejectsLyingRegistryManifestHeader pins that Publish fails when the
// registry reports a Docker-Content-Digest header that disagrees with the
// returned bytes, even though those bytes match the pushed digest.
func TestPublishRejectsLyingRegistryManifestHeader(t *testing.T) {
	const repository = "kodama/fixture-publish-lying-header"
	body := []byte(`{"schemaVersion":2,"content":"pushed"}`)
	pushed := DigestOf(body)
	lie := DigestOf([]byte(`{"schemaVersion":2,"content":"lie"}`))
	instance := fixtureStubManifestServer(t, repository, body, lie)
	builder := NewFixtureBuilder(WithFixtureRunner(newFixturePushRunner(pushed)), WithFixtureEnv([]string{"PATH=/bin"}))
	image := FixtureImage{Tag: "localhost/kodama-fixture-publish:first", ImageID: fixtureTestBaseDigest}
	if _, err := builder.Publish(context.Background(), instance, image, repository, "current"); err == nil {
		t.Fatalf("publish accepted a lying Docker-Content-Digest %q for honest bytes %q", lie, pushed)
	}
}

// TestFixtureNamesAreShardScoped pins twin safety: two shard IDs never share a
// repository, local tag or local prefix. Each shard's names stay inside its own
// prefix, the cleanup guard accepts both names of each pair, and cleaning one
// shard's builder never issues a removal for the other shard's names.
func TestFixtureNamesAreShardScoped(t *testing.T) {
	const shardA, shardB = "shard-aaaa", "shard-bbbb"
	host := "127.0.0.1:5000"

	repoA := FixtureRepository(shardA, "rebuild")
	repoB := FixtureRepository(shardB, "rebuild")
	tagA := FixtureLocalTag(shardA, "first")
	tagB := FixtureLocalTag(shardB, "first")
	prefixA := FixtureLocalPrefix(shardA)
	prefixB := FixtureLocalPrefix(shardB)
	if repoA == repoB {
		t.Fatalf("two shards share repository %q", repoA)
	}
	if tagA == tagB {
		t.Fatalf("two shards share local tag %q", tagA)
	}
	if prefixA == prefixB {
		t.Fatalf("two shards share local prefix %q", prefixA)
	}
	// Two different suffixes within one shard are distinct names too: a twin
	// fixture with another suffix must never collide with this one.
	if first, second := FixtureLocalTag(shardA, "first"), FixtureLocalTag(shardA, "second"); first == second {
		t.Fatalf("two suffixes share local tag %q", first)
	}
	if first, second := FixtureRepository(shardA, "first"), FixtureRepository(shardA, "second"); first == second {
		t.Fatalf("two suffixes share repository %q", first)
	}
	registryA := host + "/" + repoA + ":current"
	registryB := host + "/" + repoB + ":current"

	// Each shard's local build tag belongs to that shard's local prefix and not
	// the twin's. The registry tags are likewise prefix-disjoint on their own
	// repository.
	for _, local := range []struct {
		name, prefix, twinPrefix string
	}{
		{tagA, prefixA, prefixB},
		{tagB, prefixB, prefixA},
	} {
		if !strings.HasPrefix(local.name, local.prefix) {
			t.Fatalf("local name %q does not start with its prefix %q", local.name, local.prefix)
		}
		if strings.HasPrefix(local.name, local.twinPrefix) {
			t.Fatalf("local name %q starts with the twin prefix %q", local.name, local.twinPrefix)
		}
	}
	for _, registry := range []struct {
		name, repository, twinRepository string
	}{
		{registryA, repoA, repoB},
		{registryB, repoB, repoA},
	} {
		if !strings.HasPrefix(registry.name, host+"/"+registry.repository) {
			t.Fatalf("registry name %q does not start with its repository %q", registry.name, registry.repository)
		}
		if strings.HasPrefix(registry.name, host+"/"+registry.twinRepository) {
			t.Fatalf("registry name %q starts with the twin repository %q", registry.name, registry.twinRepository)
		}
	}

	// The removal guard accepts both names of each pair.
	for _, name := range []string{tagA, registryA, tagB, registryB} {
		if !fixtureNameOwned(name) {
			t.Fatalf("fixture name %q was refused by the cleanup guard", name)
		}
	}

	// A builder for shard A removes only its own names, never the twin's.
	runner := &recordingRunner{}
	builder := NewFixtureBuilder(WithFixtureRunner(runner), WithFixtureEnv([]string{"PATH=/bin"}))
	builder.trackCreated(tagA, registryA)
	if err := builder.Cleanup(context.Background()); err != nil {
		t.Fatalf("cleanup shard A: %v", err)
	}
	for _, command := range runner.recorded() {
		if len(command) == 2 && (command[1] == tagB || command[1] == registryB) {
			t.Fatalf("shard A cleanup removed twin name %q", command[1])
		}
	}
	if got, want := len(runner.recorded()), 2; got != want {
		t.Fatalf("shard A cleanup issued %d commands; want its own %d: %v", got, want, runner.recorded())
	}
}

// TestFixtureNameGuardRejectsForeignNames pins the namespace shape of the
// cleanup guard: a bare localhost/ name or a fixture-looking basename under
// another repository must be refused, not merely a leading fixture- string.
func TestFixtureNameGuardRejectsForeignNames(t *testing.T) {
	accepted := []string{
		"localhost/kodama-fixture-shard:first",
		"127.0.0.1:5000/kodama/fixture-shard-rebuild:current",
		"127.0.0.1:5000/kodama/fixture-shard-rebuild@" + fixtureTestBaseDigest,
		"[::1]:5001/kodama/fixture-shard:tag",
	}
	for _, name := range accepted {
		if !fixtureNameOwned(name) {
			t.Fatalf("fixture name %q was refused by the cleanup guard", name)
		}
	}
	refused := []string{
		"localhost/other",
		"localhost/kodama-other:tag",
		"127.0.0.1:5000/other/fixture-x",
		"127.0.0.1:5000/other/fixture-x:tag",
		"127.0.0.1:5000/other/repo:current",
		"127.0.0.1:5000/kodama/fixture-x/extra:tag",
		"kodama/fixture-x:tag",
		FixtureBaseTag,
		" localhost/kodama-fixture-shard:tag",
		"localhost/kodama-fixture-shard:tag ",
	}
	for _, name := range refused {
		if fixtureNameOwned(name) {
			t.Fatalf("foreign name %q was accepted by the cleanup guard", name)
		}
	}
}

// TestFixtureRunAndPushArgsPinControls pins the exact run and push vectors:
// a digest launch always pulls, the workload has no network, and TLS
// verification is disabled only for these harness-registry commands. Any
// dropped or weakened flag changes the vector and fails here.
func TestFixtureRunAndPushArgsPinControls(t *testing.T) {
	reference := "127.0.0.1:5000/kodama/fixture-args@" + fixtureTestBaseDigest
	name := runtimesupport.RunScoped("kodama-fixture-run-1")
	gotRun := fixtureRunArgs(reference, name, runtimesupport.RunLabels())
	requireFixtureArgsEqual(t, gotRun, fixtureWantRunArgs(reference, name))
	requireFixtureProbeScope(t, gotRun)
	wantPush := []string{
		"push", "--tls-verify=false", "--digestfile", "/tmp/digest",
		"localhost/kodama-fixture-args:first", reference,
	}
	if got := fixturePushArgs("localhost/kodama-fixture-args:first", reference, "/tmp/digest"); strings.Join(got, " ") != strings.Join(wantPush, " ") {
		t.Fatalf("push arguments = %q; want %q", strings.Join(got, " "), strings.Join(wantPush, " "))
	}
}

// TestFixtureGitVersionPinsTheLocalGitProbe pins the git probe the factory
// fixture needs: it runs only inside this fixture's own local build tag, never
// pulls (the base was already resolved once), keeps the workload offline and
// overrides the marker entrypoint with git. A foreign image, a tag-only
// reference and a run that does not actually print a git version are refused.
func TestFixtureGitVersionPinsTheLocalGitProbe(t *testing.T) {
	const localTag = "localhost/kodama-fixture-unit:git"
	name := runtimesupport.RunScoped("kodama-fixture-git-1")
	gotArgs := fixtureGitArgs(localTag, name, runtimesupport.RunLabels())
	requireFixtureArgsEqual(t, gotArgs, fixtureWantGitArgs(localTag, name))
	requireFixtureProbeScope(t, gotArgs)

	runner := &recordingRunner{}
	runner.handler = func(command harness.Command) (harness.Result, error) {
		if len(command.Args) > 0 && command.Args[0] == "run" {
			return harness.Result{Stdout: []byte("git version 2.47.3\n")}, nil
		}
		return harness.Result{}, nil
	}
	builder := NewFixtureBuilder(WithFixtureRunner(runner), WithFixtureEnv([]string{"PATH=/bin"}))
	version, err := builder.GitVersion(context.Background(), localTag)
	if err != nil {
		t.Fatalf("run the git probe: %v", err)
	}
	if version != "git version 2.47.3" {
		t.Fatalf("git probe returned %q; want the reported version", version)
	}
	issued := runner.recorded()
	if len(issued) != 1 {
		t.Fatalf("git probe issued %v; want exactly one command", issued)
	}
	issuedName := commandArg(issued[0], "--name")
	requireFixtureArgsEqual(t, issued[0], fixtureWantGitArgs(localTag, issuedName))
	requireFixtureProbeScope(t, issued[0])

	// The probe is confined to this fixture's local namespace; a foreign image
	// must be refused before any command runs.
	for _, reference := range []string{
		"docker.io/library/buildpack-deps:trixie-scm",
		"127.0.0.1:5000/kodama/fixture-unit:git",
		"localhost/other:git",
		" localhost/kodama-fixture-unit:git",
	} {
		if _, err := builder.GitVersion(context.Background(), reference); err == nil {
			t.Fatalf("git probe accepted non-fixture image %q", reference)
		}
	}
	if after := runner.recorded(); len(after) != len(issued) {
		t.Fatalf("a refused git probe still issued commands: %v", after[len(issued):])
	}

	// A run that exits zero but does not report a git version is a failure, not
	// a pass: the fixture must really provide the binary.
	runner.handler = func(command harness.Command) (harness.Result, error) {
		return harness.Result{Stdout: []byte("sh: git: not found\n")}, nil
	}
	if _, err := builder.GitVersion(context.Background(), localTag); err == nil {
		t.Fatal("git probe accepted output that is not a git version")
	}
}

// TestRunMarkerRefusesForeignReferences pins that --tls-verify=false is
// confined to the exact harness registry host, not a prefix, suffix or
// substring of it: a foreign or ambiguous host, a tag-only reference and an
// uppercase or malformed reference are each refused before any command runs.
func TestRunMarkerRefusesForeignReferences(t *testing.T) {
	instance := &Registry{BaseURL: "http://127.0.0.1:5000"}
	runner := &recordingRunner{}
	builder := NewFixtureBuilder(WithFixtureRunner(runner), WithFixtureEnv([]string{"PATH=/bin"}))
	refused := []string{
		"127.0.0.1:5001/kodama/fixture-shard:tag",
		"localhost/kodama-fixture-shard:tag",
		"kodama-fixture-shard:tag",
		"127.0.0.1:5000/kodama/Fixture-Shard:tag",
		"127.0.0.1:5000/kodama/fixture-shard:TAG",
		"127.0.0.1:5000/kodama/fixture-shard@sha256:nothex",
		" 127.0.0.1:5000/kodama/fixture-shard:tag",
		// Host matching must be exact equality: a port that merely extends the
		// real one, an evil name that embeds the real host, a prefixed or
		// userinfo-wrapped host, a doubled slash, another loopback spelling and
		// a scheme prefix are all foreign.
		"127.0.0.1:50001/kodama/fixture-x:t",
		"127.0.0.1:5000.evil.example/kodama/fixture-x:t",
		"x127.0.0.1:5000/kodama/fixture-x:t",
		"user@127.0.0.1:5000/kodama/fixture-x:t",
		"127.0.0.1:5000//kodama/fixture-x:t",
		"127.0.0.1:5000/kodama//fixture-x:t",
		"[::1]:5000/kodama/fixture-x:t",
		"localhost:5000/kodama/fixture-x:t",
		"docker://127.0.0.1:5000/kodama/fixture-x:t",
	}
	for _, reference := range refused {
		if _, err := builder.RunMarker(context.Background(), instance, reference); err == nil {
			t.Fatalf("run accepted foreign reference %q", reference)
		}
		if commands := runner.recorded(); len(commands) != 0 {
			t.Fatalf("run refused %q but still issued commands: %v", reference, commands)
		}
	}
	if commands := runner.recorded(); len(commands) != 0 {
		t.Fatalf("a refused run still issued commands: %v", commands)
	}
	if _, err := builder.RunMarker(context.Background(), nil, "127.0.0.1:5000/kodama/fixture-shard:tag"); err == nil {
		t.Fatal("run accepted a nil registry")
	}
}

// TestRunMarkerIssuesPinnedRunForRegistryReference pins the actual command
// RunMarker issues: the passed reference is launched verbatim (never rewritten
// to a mutable :current tag) with the fixed run controls.
func TestRunMarkerIssuesPinnedRunForRegistryReference(t *testing.T) {
	instance := &Registry{BaseURL: "http://127.0.0.1:5000"}
	reference := instance.Host() + "/kodama/fixture-run@" + fixtureTestBaseDigest
	runner := &recordingRunner{}
	runner.handler = func(command harness.Command) (harness.Result, error) {
		if len(command.Args) > 0 && command.Args[0] == "run" {
			return harness.Result{Stdout: []byte("fixture-run\n")}, nil
		}
		return harness.Result{}, nil
	}
	builder := NewFixtureBuilder(WithFixtureRunner(runner), WithFixtureEnv([]string{"PATH=/bin"}))
	marker, err := builder.RunMarker(context.Background(), instance, reference)
	if err != nil {
		t.Fatalf("run fixture reference: %v", err)
	}
	if marker != "fixture-run" {
		t.Fatalf("run marker = %q; want %q", marker, "fixture-run")
	}
	commands := runner.recorded()
	if len(commands) != 1 {
		t.Fatalf("run issued %d commands; want 1: %v", len(commands), commands)
	}
	issuedName := commandArg(commands[0], "--name")
	requireFixtureArgsEqual(t, commands[0], fixtureWantRunArgs(reference, issuedName))
	requireFixtureProbeScope(t, commands[0])
}

// fixtureWantContainerIdentity reconstructs the exact name-and-label segment of
// a probe command from the identity values the builder is given. The labels are
// written in their sorted order rather than derived by re-sorting RunLabels, so
// a dropped flag, an added flag, a changed label value or a reordering all
// change the vector this pins.
func fixtureWantContainerIdentity(name string) []string {
	return []string{
		"--name", name,
		"--label", harness.LabelOwnerPID + "=" + runtimesupport.RunOwnerPID(),
		"--label", harness.LabelTest + "=1",
	}
}

// fixtureWantRunArgs is the full expected run vector: fixed controls, the
// run-scoped identity, then the reference.
func fixtureWantRunArgs(reference, name string) []string {
	args := []string{"run", "--rm", "--pull=always", "--tls-verify=false", "--network=none"}
	args = append(args, fixtureWantContainerIdentity(name)...)
	return append(args, reference)
}

// fixtureWantGitArgs is the full expected Git probe vector: fixed controls, the
// run-scoped identity, then the entrypoint-and-tag action tail.
func fixtureWantGitArgs(localTag, name string) []string {
	args := []string{"run", "--rm", "--pull=never", "--network=none"}
	args = append(args, fixtureWantContainerIdentity(name)...)
	return append(args, "--entrypoint", "git", localTag, "--version")
}

// requireFixtureArgsEqual compares the whole argument vector, so a missing
// control, an extra flag such as --privileged or -v /:/host, or a reordered
// tail fails rather than passing a membership check.
func requireFixtureArgsEqual(t *testing.T, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("arguments = %q; want exactly %q", strings.Join(got, " "), strings.Join(want, " "))
	}
}

// requireFixtureProbeScope pins that a short-lived probe container carries an
// explicit run-scoped --name and this run's cleanup labels. Without them an
// interrupted probe leaks an auto-named container that no sweep can attribute.
func requireFixtureProbeScope(t *testing.T, args []string) {
	t.Helper()
	name := ""
	for index, arg := range args {
		if arg == "--name" {
			if index+1 >= len(args) || strings.TrimSpace(args[index+1]) == "" {
				t.Fatalf("probe arguments have no --name value: %v", args)
			}
			name = args[index+1]
		}
	}
	if name == "" {
		t.Fatalf("probe arguments have no --name: %v", args)
	}
	if !strings.Contains(name, runtimesupport.RunScope().Suffix()) {
		t.Fatalf("probe container name %q is not run-scoped (%q)", name, runtimesupport.RunScope().Suffix())
	}
	labels := map[string]string{}
	for index, arg := range args {
		if arg != "--label" || index+1 >= len(args) {
			continue
		}
		key, value, ok := strings.Cut(args[index+1], "=")
		if !ok {
			t.Fatalf("probe label %q is not key=value", args[index+1])
		}
		labels[key] = value
	}
	if !runtimesupport.OwnedByRun(labels) {
		t.Fatalf("probe labels %v are not this run's cleanup scope", labels)
	}
}
