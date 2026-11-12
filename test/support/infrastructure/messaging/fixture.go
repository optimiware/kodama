package messaging

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/optimiware/kodama/test/support/infrastructure/harness"
	runtimesupport "github.com/optimiware/kodama/test/support/infrastructure/runtime"
)

// The deterministic fixture image is the runnable artifact later recipe tests
// publish through the real local registry. It prints a marker by default and is
// built on a Git-capable SCM base so the protected factory can run the real Git
// service against it; the build itself still proves a reproducible
// manifest-digest flow and tag-retarget immunity.
//
// Reproducibility controls:
//   - the Containerfile names exactly one digest-pinned base
//     (repository:tag@sha256:...); the caller resolves that tag once and Build
//     refuses any resolution that disagrees with the committed pin;
//   - build context bytes are normalized into a descriptor whose extra files
//     are sorted, so enumeration order and source mtimes cannot leak in;
//   - materialized files get the fixed epoch mtime and fixed permission bits;
//   - the build runs with --pull=never, --network=none (no package
//     installation), --no-cache, --timestamp 0 and SOURCE_DATE_EPOCH=0.
const (
	// FixtureBaseTag is the pinned Git-capable SCM base of the deterministic
	// fixture. The Containerfile names the same tag with FixtureBaseDigest; the
	// tag is resolved once per shard and only the resulting immutable reference
	// is ever built.
	FixtureBaseTag = "docker.io/library/buildpack-deps:trixie-scm"

	// FixtureBaseDigest is the manifest digest FixtureBaseTag resolved to when
	// the pin was recorded. The Containerfile carries exactly this digest, and
	// the component suite fails when the registry serves a different one.
	FixtureBaseDigest = "sha256:712f969c9d81a35f9f00cce701faae521891d42a20b592b61bd0e6c5328fad7c"

	// FixtureEpochUnix is the fixed build timestamp for image config and layer
	// entries.
	FixtureEpochUnix int64 = 0

	// FixtureFile names inside the generated build context.
	FixtureContainerfilePath = "Containerfile"
	FixtureScriptPath        = "fixture.sh"
	FixtureMarkerPath        = "marker"

	fixtureSourceDateEpoch = "SOURCE_DATE_EPOCH"

	fixtureLocalTagPrefix = "localhost/kodama-fixture-"
	fixtureRepositoryRoot = "kodama/fixture-"
)

//go:embed Containerfile fixture.sh
var fixtureAssets embed.FS

// fixtureContainerfile and fixtureScript are the canonical fixture source
// bytes. They are embedded so normalization, materialization and direct builds
// all use exactly the files committed beside this code.
var (
	fixtureContainerfile = mustFixtureAsset(FixtureContainerfilePath)
	fixtureScript        = mustFixtureAsset(FixtureScriptPath)
)

func mustFixtureAsset(name string) []byte {
	data, err := fixtureAssets.ReadFile(name)
	if err != nil {
		panic(fmt.Sprintf("messaging: read embedded fixture asset %s: %v", name, err))
	}
	return data
}

// FixtureFile is one byte file that participates in the build context.
type FixtureFile struct {
	Path    string
	Content []byte
	Mode    os.FileMode
}

// FixtureInputs describes one deterministic fixture build. BaseReference must
// already be the immutable repository@sha256:... reference resolved by the
// caller.
type FixtureInputs struct {
	BaseReference string
	Marker        string
	ExtraFiles    []FixtureFile
}

// NormalizedFile is the content-addressed identity of one extra file.
type NormalizedFile struct {
	Path   string `json:"path"`
	Mode   uint32 `json:"mode"`
	Size   int    `json:"size"`
	Digest string `json:"digest"`
}

// NormalizedBuildInputs is the stable identity of one fixture build.
// Descriptor hashes the immutable base, the embedded Containerfile and script,
// the marker bytes and the sorted extra-file descriptors; it therefore ignores
// source enumeration order and source mtimes.
type NormalizedBuildInputs struct {
	BaseReference       string           `json:"base_reference"`
	ContainerfileDigest string           `json:"containerfile_digest"`
	ScriptDigest        string           `json:"script_digest"`
	MarkerDigest        string           `json:"marker_digest"`
	Files               []NormalizedFile `json:"extra_files"`

	// Descriptor is the sha256 identity of the canonical fields above. It is
	// not part of its own input.
	Descriptor string `json:"-"`

	marker     []byte
	extraFiles []FixtureFile
}

// NormalizeBaseReference validates an immutable image reference. A naked tag
// is refused: the caller must resolve the tag once and pass the digest. A tag
// carried beside a digest (repo:tag@sha256:...) is accepted and reduced to
// repo@sha256:..., so it cannot act as a second identity.
func NormalizeBaseReference(reference string) (string, error) {
	trimmed := strings.TrimSpace(reference)
	at := strings.LastIndex(trimmed, "@")
	if at <= 0 || strings.Contains(trimmed[at+1:], "@") {
		return "", fmt.Errorf("image reference %q is not digest-pinned; resolve the tag once and pass repository@sha256:<digest>", reference)
	}
	repository := trimmed[:at]
	digest := trimmed[at+1:]
	if colon := strings.LastIndex(repository, ":"); colon > strings.LastIndex(repository, "/") {
		repository = repository[:colon]
	}
	if repository == "" || strings.ContainsAny(repository, " \t\r\n@") {
		return "", fmt.Errorf("image reference %q has an empty or malformed repository", reference)
	}
	if !validFixtureDigest(digest) {
		return "", fmt.Errorf("image reference %q has an invalid sha256 digest", reference)
	}
	return repository + "@" + digest, nil
}

// validFixtureDigest requires a canonical lowercase sha256 digest.
func validFixtureDigest(digest string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(digest, prefix) {
		return false
	}
	raw := digest[len(prefix):]
	if len(raw) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(raw)
	if err != nil || hex.EncodeToString(decoded) != raw {
		return false
	}
	return true
}

// parseFixtureBase extracts the single base reference from a Containerfile.
// Exactly one FROM is allowed: a multi-stage recipe, a FROM flag or a
// build-argument base would let the image use a base the committed pin does not
// name. The returned reference is normalized to repository@sha256:... .
func parseFixtureBase(containerfile []byte) (FixtureBase, error) {
	var froms []string
	for index, line := range strings.Split(string(containerfile), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) == 0 || !strings.EqualFold(fields[0], "FROM") {
			continue
		}
		if len(fields) != 2 {
			return FixtureBase{}, fmt.Errorf("line %d: FROM must be exactly 'FROM <reference>'", index+1)
		}
		froms = append(froms, fields[1])
	}
	if len(froms) != 1 {
		return FixtureBase{}, fmt.Errorf("found %d FROM instructions; want exactly one digest-pinned base", len(froms))
	}
	reference, err := NormalizeBaseReference(froms[0])
	if err != nil {
		return FixtureBase{}, err
	}
	return FixtureBase{Digest: reference[strings.LastIndex(reference, "@")+1:], Reference: reference}, nil
}

// PinnedFixtureBase resolves the base the committed Containerfile pins and
// refuses a Containerfile whose single FROM has drifted from FixtureBaseTag and
// FixtureBaseDigest. Build uses this value, so the base identity in the
// descriptor and the image recipe cannot disagree.
func PinnedFixtureBase() (FixtureBase, error) {
	return pinnedFixtureBase(fixtureContainerfile)
}

// pinnedFixtureBase is PinnedFixtureBase with an explicit Containerfile so the
// drift check itself is unit-testable.
func pinnedFixtureBase(containerfile []byte) (FixtureBase, error) {
	pinned, err := NewFixtureBase(FixtureBaseTag, FixtureBaseDigest)
	if err != nil {
		return FixtureBase{}, fmt.Errorf("pinned fixture base: %w", err)
	}
	parsed, err := parseFixtureBase(containerfile)
	if err != nil {
		return FixtureBase{}, fmt.Errorf("fixture Containerfile: %w", err)
	}
	if parsed.Reference != pinned.Reference {
		return FixtureBase{}, fmt.Errorf("fixture Containerfile base %q does not match the pinned %q", parsed.Reference, pinned.Reference)
	}
	return pinned, nil
}

// NormalizeBuildInputs reduces one fixture specification to its stable build
// identity. It refuses a mutable base, reserved or unsafe extra paths and an
// empty marker.
func NormalizeBuildInputs(inputs FixtureInputs) (NormalizedBuildInputs, error) {
	baseReference, err := NormalizeBaseReference(inputs.BaseReference)
	if err != nil {
		return NormalizedBuildInputs{}, err
	}
	if inputs.Marker == "" {
		return NormalizedBuildInputs{}, errors.New("fixture marker is empty")
	}
	files := make([]FixtureFile, 0, len(inputs.ExtraFiles))
	seen := map[string]bool{}
	for _, file := range inputs.ExtraFiles {
		cleaned, pathErr := normalizeFixturePath(file.Path)
		if pathErr != nil {
			return NormalizedBuildInputs{}, pathErr
		}
		switch cleaned {
		case FixtureContainerfilePath, FixtureScriptPath, FixtureMarkerPath:
			return NormalizedBuildInputs{}, fmt.Errorf("fixture extra path %q is reserved", cleaned)
		}
		if seen[cleaned] {
			return NormalizedBuildInputs{}, fmt.Errorf("fixture extra path %q is duplicated", cleaned)
		}
		seen[cleaned] = true
		mode := file.Mode.Perm()
		if mode == 0 {
			mode = 0o644
		}
		files = append(files, FixtureFile{
			Path:    cleaned,
			Content: append([]byte(nil), file.Content...),
			Mode:    mode,
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	normalizedFiles := make([]NormalizedFile, 0, len(files))
	for _, file := range files {
		normalizedFiles = append(normalizedFiles, NormalizedFile{
			Path:   file.Path,
			Mode:   uint32(file.Mode.Perm()),
			Size:   len(file.Content),
			Digest: DigestOf(file.Content),
		})
	}

	normalized := NormalizedBuildInputs{
		BaseReference:       baseReference,
		ContainerfileDigest: DigestOf(fixtureContainerfile),
		ScriptDigest:        DigestOf(fixtureScript),
		MarkerDigest:        DigestOf([]byte(inputs.Marker)),
		Files:               normalizedFiles,
		marker:              []byte(inputs.Marker),
		extraFiles:          files,
	}
	descriptor, err := fixtureDescriptor(normalized)
	if err != nil {
		return NormalizedBuildInputs{}, err
	}
	normalized.Descriptor = descriptor
	return normalized, nil
}

// normalizeFixturePath accepts a clean relative slash path inside the build
// context.
func normalizeFixturePath(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", errors.New("fixture extra path is empty")
	}
	if strings.ContainsRune(value, 0) || strings.ContainsRune(value, '\\') {
		return "", fmt.Errorf("fixture extra path %q is not a portable relative path", value)
	}
	cleaned := path.Clean(value)
	if cleaned != value || cleaned == "." || path.IsAbs(cleaned) || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("fixture extra path %q is not a clean relative path", value)
	}
	return cleaned, nil
}

// fixtureDescriptor hashes the canonical normalized fields. Unexported content
// and Descriptor itself are not part of the identity.
func fixtureDescriptor(inputs NormalizedBuildInputs) (string, error) {
	type identity struct {
		BaseReference       string           `json:"base_reference"`
		ContainerfileDigest string           `json:"containerfile_digest"`
		ScriptDigest        string           `json:"script_digest"`
		MarkerDigest        string           `json:"marker_digest"`
		Files               []NormalizedFile `json:"extra_files"`
	}
	payload, err := json.Marshal(identity{
		BaseReference:       inputs.BaseReference,
		ContainerfileDigest: inputs.ContainerfileDigest,
		ScriptDigest:        inputs.ScriptDigest,
		MarkerDigest:        inputs.MarkerDigest,
		Files:               inputs.Files,
	})
	if err != nil {
		return "", fmt.Errorf("encode normalized fixture inputs: %w", err)
	}
	return DigestOf(payload), nil
}

// WriteContext materializes the normalized inputs into dir with fixed mtimes
// and modes. Callers must treat dir as disposable.
func (inputs NormalizedBuildInputs) WriteContext(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return errors.New("fixture build context directory is empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create fixture build context: %w", err)
	}
	if err := writeFixtureContextFile(dir, FixtureContainerfilePath, fixtureContainerfile, 0o644); err != nil {
		return err
	}
	if err := writeFixtureContextFile(dir, FixtureScriptPath, fixtureScript, 0o555); err != nil {
		return err
	}
	if err := writeFixtureContextFile(dir, FixtureMarkerPath, inputs.marker, 0o444); err != nil {
		return err
	}
	for _, file := range inputs.extraFiles {
		if err := writeFixtureContextFile(dir, file.Path, file.Content, file.Mode.Perm()); err != nil {
			return err
		}
	}
	if err := stampFixtureContextTree(dir); err != nil {
		return err
	}
	return nil
}

// stampFixtureContextTree fixes every directory and file timestamp after all
// writes. Nested extra files create parent directories whose ambient mtimes
// would otherwise enter the layer tar.
func stampFixtureContextTree(root string) error {
	epoch := time.Unix(FixtureEpochUnix, 0)
	var directories []string
	if err := filepath.WalkDir(root, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			directories = append(directories, current)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("walk fixture build context: %w", err)
	}
	for _, directory := range directories {
		if err := os.Chmod(directory, 0o755); err != nil {
			return fmt.Errorf("set fixture context directory mode for %s: %w", directory, err)
		}
		if err := os.Chtimes(directory, epoch, epoch); err != nil {
			return fmt.Errorf("stamp fixture context directory %s: %w", directory, err)
		}
	}
	return nil
}

func writeFixtureContextFile(dir, relative string, content []byte, mode os.FileMode) error {
	full := filepath.Join(dir, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("create fixture context directory for %s: %w", relative, err)
	}
	if err := os.WriteFile(full, content, mode); err != nil {
		return fmt.Errorf("write fixture context file %s: %w", relative, err)
	}
	if err := os.Chmod(full, mode); err != nil {
		return fmt.Errorf("set fixture context mode for %s: %w", relative, err)
	}
	epoch := time.Unix(FixtureEpochUnix, 0)
	if err := os.Chtimes(full, epoch, epoch); err != nil {
		return fmt.Errorf("stamp fixture context file %s: %w", relative, err)
	}
	return nil
}

// ReadFixtureFiles loads one source directory into FixtureFiles. Only the
// bytes, relative slash path and permission bits are retained: source mtimes
// are deliberately dropped.
func ReadFixtureFiles(dir string) ([]FixtureFile, error) {
	files := make([]FixtureFile, 0, 4)
	err := filepath.WalkDir(dir, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(dir, current)
		if err != nil {
			return fmt.Errorf("relativize fixture file %s: %w", current, err)
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stat fixture file %s: %w", current, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("fixture file %s is not a regular file", current)
		}
		content, err := os.ReadFile(current)
		if err != nil {
			return fmt.Errorf("read fixture file %s: %w", current, err)
		}
		files = append(files, FixtureFile{
			Path:    filepath.ToSlash(relative),
			Content: content,
			Mode:    info.Mode().Perm(),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// FingerprintBuildContext hashes one materialized context by sorted path,
// mode, mtime and content. It is the observation used to prove two
// materializations are byte-for-byte identical, including their fixed
// timestamps.
func FingerprintBuildContext(dir string) (string, error) {
	type entry struct {
		Path   string
		Mode   os.FileMode
		MTime  int64
		Digest string
	}
	entries := make([]entry, 0, 4)
	err := filepath.WalkDir(dir, func(current string, item os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if item.IsDir() {
			info, err := item.Info()
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(dir, current)
			if err != nil {
				return err
			}
			entries = append(entries, entry{
				Path:  filepath.ToSlash(relative),
				Mode:  info.Mode().Perm(),
				MTime: info.ModTime().UnixNano(),
			})
			return nil
		}
		relative, err := filepath.Rel(dir, current)
		if err != nil {
			return err
		}
		info, err := item.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("fixture context entry %s is not a regular file", current)
		}
		content, err := os.ReadFile(current)
		if err != nil {
			return err
		}
		entries = append(entries, entry{
			Path:   filepath.ToSlash(relative),
			Mode:   info.Mode().Perm(),
			MTime:  info.ModTime().UnixNano(),
			Digest: DigestOf(content),
		})
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("fingerprint fixture context: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	hash := sha256.New()
	for _, item := range entries {
		_, _ = fmt.Fprintf(hash, "%s\x00%o\x00%d\x00%s\x00", item.Path, item.Mode, item.MTime, item.Digest)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

// FixtureBase is the single immutable base resolution a build is allowed to
// use.
type FixtureBase struct {
	Tag       string
	Digest    string
	Reference string
}

// NewFixtureBase builds the immutable base reference from a tag resolved once
// by the caller.
func NewFixtureBase(tag, digest string) (FixtureBase, error) {
	trimmedTag := strings.TrimSpace(tag)
	if trimmedTag == "" {
		return FixtureBase{}, errors.New("fixture base tag is empty")
	}
	digest = strings.TrimSpace(digest)
	if !validFixtureDigest(digest) {
		return FixtureBase{}, fmt.Errorf("fixture base digest %q is not a canonical sha256 digest", digest)
	}
	repository := trimmedTag
	if colon := strings.LastIndex(repository, ":"); colon > strings.LastIndex(repository, "/") {
		repository = repository[:colon]
	}
	if repository == "" {
		return FixtureBase{}, fmt.Errorf("fixture base tag %q has no repository", tag)
	}
	if strings.ContainsAny(repository, " \t\r\n@") {
		return FixtureBase{}, fmt.Errorf("fixture base tag %q has a malformed repository", tag)
	}
	return FixtureBase{
		Tag:       trimmedTag,
		Digest:    digest,
		Reference: repository + "@" + digest,
	}, nil
}

// FixtureImage is one local, reproducibly built fixture image.
type FixtureImage struct {
	Base      FixtureBase
	Inputs    NormalizedBuildInputs
	Tag       string
	ImageID   string
	Toolchain string
}

// FixturePublication is one fixture image published through the real registry.
type FixturePublication struct {
	Repository string
	Tag        string
	// Reference is the mutable registry tag reference that was published.
	Reference string
	// RunnableReference is the immutable registry repository@digest reference.
	RunnableReference string
	// ManifestDigest is the registry manifest digest reported by the push.
	ManifestDigest string
	// RegistryManifestDigest is the digest recomputed from the registry's own
	// returned manifest bytes; Publish fails unless it matches ManifestDigest.
	RegistryManifestDigest string
	ImageID                string
}

// FixtureBuilder runs the real Podman build/push/run commands. The Runner seam
// exists for focused unit tests of argument construction; production component
// tests use the default ExecRunner.
type FixtureBuilder struct {
	binary string
	runner harness.Runner
	env    []string
	logf   func(format string, args ...any)

	buildLimit time.Duration
	pushLimit  time.Duration
	runLimit   time.Duration

	// mu guards the exact local image names this fixture created or pulled.
	// Cleanup removes those names, never an image ID, and never anything the
	// fixture did not create.
	mu           sync.Mutex
	created      map[string]struct{}
	createdOrder []string
}

// FixtureBuilderOption configures a FixtureBuilder.
type FixtureBuilderOption func(*FixtureBuilder)

// WithFixtureRunner replaces the process runner.
func WithFixtureRunner(runner harness.Runner) FixtureBuilderOption {
	return func(builder *FixtureBuilder) {
		if runner != nil {
			builder.runner = runner
		}
	}
}

// WithFixtureEnv replaces the explicit command environment. The deterministic
// SOURCE_DATE_EPOCH=0 control is always applied afterwards.
func WithFixtureEnv(env []string) FixtureBuilderOption {
	return func(builder *FixtureBuilder) {
		if env == nil {
			builder.env = harness.BaseEnv()
			return
		}
		builder.env = append([]string(nil), env...)
	}
}

// WithFixtureLogger receives human-readable build and publication records.
func WithFixtureLogger(f func(format string, args ...any)) FixtureBuilderOption {
	return func(builder *FixtureBuilder) {
		if f != nil {
			builder.logf = f
		}
	}
}

// NewFixtureBuilder builds a deterministic fixture builder.
func NewFixtureBuilder(opts ...FixtureBuilderOption) *FixtureBuilder {
	builder := &FixtureBuilder{
		binary:     "podman",
		runner:     harness.ExecRunner{},
		env:        harness.BaseEnv(),
		logf:       func(string, ...any) {},
		buildLimit: 10 * time.Minute,
		pushLimit:  5 * time.Minute,
		runLimit:   3 * time.Minute,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(builder)
		}
	}
	if builder.env == nil {
		builder.env = harness.BaseEnv()
	}
	builder.env = fixtureBuildEnv(builder.env)
	return builder
}

// fixtureBuildEnv pins SOURCE_DATE_EPOCH regardless of the supplied
// environment, so a caller cannot accidentally reintroduce ambient timestamps.
func fixtureBuildEnv(env []string) []string {
	out := make([]string, 0, len(env)+1)
	for _, value := range env {
		if strings.HasPrefix(value, fixtureSourceDateEpoch+"=") {
			continue
		}
		out = append(out, value)
	}
	out = append(out, fixtureSourceDateEpoch+"="+strconv.FormatInt(FixtureEpochUnix, 10))
	return out
}

// command runs one Podman invocation with the explicit environment and a
// bounded operation timeout. No shell is involved.
func (builder *FixtureBuilder) command(ctx context.Context, limit time.Duration, args ...string) (harness.Result, error) {
	if limit > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, limit)
		defer cancel()
	}
	env := append([]string(nil), builder.env...)
	if env == nil {
		env = harness.BaseEnv()
	}
	return builder.runner.Run(ctx, harness.Command{
		Name: builder.binary,
		Args: append([]string(nil), args...),
		Env:  env,
	})
}

// ToolchainVersion reports the exact build tool version used for provenance.
func (builder *FixtureBuilder) ToolchainVersion(ctx context.Context) (string, error) {
	result, err := builder.command(ctx, 2*time.Minute, "--version")
	if err != nil {
		return "", err
	}
	version := strings.TrimSpace(string(result.Stdout))
	if version == "" {
		version = strings.TrimSpace(string(result.Stderr))
	}
	if version == "" {
		return "", errors.New("fixture build tool reported no version")
	}
	return version, nil
}

// Build performs one clean, network-disabled build from the immutable base and
// normalized inputs. The base is resolved by the caller exactly once.
func (builder *FixtureBuilder) Build(ctx context.Context, base FixtureBase, inputs FixtureInputs, localTag string) (FixtureImage, error) {
	if strings.TrimSpace(localTag) == "" {
		return FixtureImage{}, errors.New("fixture local tag is empty")
	}
	pinned, err := PinnedFixtureBase()
	if err != nil {
		return FixtureImage{}, err
	}
	normalizedBase, err := NormalizeBaseReference(base.Reference)
	if err != nil {
		return FixtureImage{}, fmt.Errorf("fixture base: %w", err)
	}
	if base.Reference != normalizedBase {
		return FixtureImage{}, fmt.Errorf("fixture base reference %q is not normalized", base.Reference)
	}
	if normalizedBase != pinned.Reference {
		return FixtureImage{}, fmt.Errorf("fixture base %q does not match the Containerfile pin %q", normalizedBase, pinned.Reference)
	}
	base = pinned
	inputs.BaseReference = pinned.Reference
	normalized, err := NormalizeBuildInputs(inputs)
	if err != nil {
		return FixtureImage{}, err
	}

	root, err := os.MkdirTemp("", "kodama-fixture-build-")
	if err != nil {
		return FixtureImage{}, fmt.Errorf("create fixture build scratch: %w", err)
	}
	defer os.RemoveAll(root)
	contextDir := filepath.Join(root, "context")
	if err := normalized.WriteContext(contextDir); err != nil {
		return FixtureImage{}, err
	}
	toolchain, err := builder.ToolchainVersion(ctx)
	if err != nil {
		return FixtureImage{}, fmt.Errorf("fixture build tool: %w", err)
	}
	// Track the build tag before the build can create it; cleanup tolerates a
	// name that a failed build never produced.
	builder.trackCreated(localTag)
	args := fixtureBuildArgs(localTag, contextDir)
	result, err := builder.command(ctx, builder.buildLimit, args...)
	if err != nil {
		return FixtureImage{}, fmt.Errorf("build fixture %s: %w: %s", localTag, err, fixtureCommandTail(result))
	}
	imageID, err := builder.imageID(ctx, localTag)
	if err != nil {
		return FixtureImage{}, err
	}
	builder.logf("messaging: built fixture %s from %s image id %s source %s", localTag, normalized.BaseReference, imageID, normalized.Descriptor)
	return FixtureImage{
		Base:      FixtureBase{Tag: base.Tag, Digest: base.Digest, Reference: normalized.BaseReference},
		Inputs:    normalized,
		Tag:       localTag,
		ImageID:   imageID,
		Toolchain: toolchain,
	}, nil
}

// fixtureBuildArgs is pure so tests can pin every determinism control. The base
// is deliberately not an argument: the Containerfile's single digest-pinned
// FROM is the only base the build may use.
func fixtureBuildArgs(localTag, contextDir string) []string {
	return []string{
		"build",
		"--pull=never",
		"--network=none",
		"--no-cache",
		"--rm",
		"--timestamp", strconv.FormatInt(FixtureEpochUnix, 10),
		"--format", "oci",
		"--tag", localTag,
		contextDir,
	}
}

func (builder *FixtureBuilder) imageID(ctx context.Context, tag string) (string, error) {
	result, err := builder.command(ctx, 2*time.Minute, "image", "inspect", "--format", "{{.Id}}", tag)
	if err != nil {
		return "", fmt.Errorf("inspect fixture image id %s: %w", tag, err)
	}
	imageID := strings.TrimSpace(string(result.Stdout))
	if imageID == "" {
		return "", fmt.Errorf("fixture image %s has no local image id", tag)
	}
	return imageID, nil
}

// Publish pushes the local fixture image to the real registry tag, then
// validates the pushed digest against the registry's own manifest bytes. The
// returned RunnableReference pins that manifest digest.
func (builder *FixtureBuilder) Publish(ctx context.Context, instance *Registry, image FixtureImage, repository, tag string) (FixturePublication, error) {
	if instance == nil {
		return FixturePublication{}, errors.New("fixture registry is nil")
	}
	if strings.TrimSpace(repository) == "" || strings.TrimSpace(tag) == "" {
		return FixturePublication{}, errors.New("fixture repository and tag are required")
	}
	reference := instance.Host() + "/" + repository + ":" + tag
	// podman push also tags the source image locally with the registry-qualified
	// reference, so that name must be cleaned up too. It is recorded before the
	// push can create it: a failed push is still cleaned up.
	builder.trackCreated(reference)
	root, err := os.MkdirTemp("", "kodama-fixture-push-")
	if err != nil {
		return FixturePublication{}, fmt.Errorf("create fixture push scratch: %w", err)
	}
	defer os.RemoveAll(root)
	digestFile := filepath.Join(root, "manifest-digest")

	result, err := builder.command(ctx, builder.pushLimit, fixturePushArgs(image.Tag, reference, digestFile)...)
	if err != nil {
		return FixturePublication{}, fmt.Errorf("publish fixture %s: %w: %s", reference, err, fixtureCommandTail(result))
	}
	rawDigest, err := os.ReadFile(digestFile)
	if err != nil {
		return FixturePublication{}, fmt.Errorf("read fixture push digest: %w", err)
	}
	manifestDigest := strings.TrimSpace(string(rawDigest))
	if !validFixtureDigest(manifestDigest) {
		return FixturePublication{}, fmt.Errorf("fixture push reported invalid manifest digest %q", manifestDigest)
	}
	registryDigest, err := RegistryManifestDigest(ctx, instance, repository, manifestDigest)
	if err != nil {
		return FixturePublication{}, fmt.Errorf("validate registry manifest for %s: %w", reference, err)
	}
	if registryDigest != manifestDigest {
		return FixturePublication{}, fmt.Errorf("registry manifest digest %q does not match the pushed manifest digest %q", registryDigest, manifestDigest)
	}
	publication := FixturePublication{
		Repository:             repository,
		Tag:                    tag,
		Reference:              reference,
		RunnableReference:      instance.Host() + "/" + repository + "@" + manifestDigest,
		ManifestDigest:         manifestDigest,
		RegistryManifestDigest: registryDigest,
		ImageID:                image.ImageID,
	}
	builder.logf("messaging: published fixture %s as %s (manifest %s)", image.Tag, reference, manifestDigest)
	return publication, nil
}

// fixturePushArgs is pure so tests can pin the push controls: TLS verification
// is disabled only for the harness registry and the digest is written to the
// caller's exact file.
func fixturePushArgs(source, reference, digestFile string) []string {
	return []string{
		"push",
		"--tls-verify=false",
		"--digestfile", digestFile,
		source,
		reference,
	}
}

// RegistryManifestDigest fetches one registry manifest and validates its
// identity: the body must hash to the requested digest (when the reference is
// a digest) and to any Docker-Content-Digest header the registry reports. It
// therefore proves digest provenance against the registry, not against a local
// image ID.
func RegistryManifestDigest(ctx context.Context, instance *Registry, repository, reference string) (string, error) {
	if instance == nil {
		return "", errors.New("fixture registry is nil")
	}
	endpoint := instance.BaseURL + "/v2/" + repository + "/manifests/" + reference
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("build registry manifest request: %w", err)
	}
	request.Header.Set("Accept", OCIManifestMediaType+", application/vnd.docker.distribution.manifest.v2+json")
	instance.authorize(request)
	client := &http.Client{Timeout: 30 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("fetch registry manifest %s: %w", reference, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("read registry manifest %s: %w", reference, err)
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry manifest %s returned %d: %s", reference, response.StatusCode, strings.TrimSpace(string(body)))
	}
	computed := DigestOf(body)
	if strings.HasPrefix(reference, "sha256:") && computed != reference {
		return "", fmt.Errorf("registry manifest bytes hash to %q, not the requested %q", computed, reference)
	}
	header := strings.TrimSpace(response.Header.Get("Docker-Content-Digest"))
	if header != "" && header != computed {
		return "", fmt.Errorf("registry manifest header %q disagrees with the returned bytes %q", header, computed)
	}
	return computed, nil
}

// fixtureProbeSequence makes every short-lived probe container name unique in
// this process. The run scope separates concurrent suite runs, and the counter
// separates probes inside one run, so `podman run --name` never collides.
var fixtureProbeSequence atomic.Uint64

// fixtureProbeName returns one run-scoped probe container name. The kind names
// the probe in a leaked-container sweep; the sequence keeps it unique.
func fixtureProbeName(kind string) string {
	return runtimesupport.RunScoped(fmt.Sprintf("kodama-fixture-%s-%d", kind, fixtureProbeSequence.Add(1)))
}

// fixtureContainerIdentityArgs renders the explicit name and this run's cleanup
// labels every probe container carries. Labels are sorted so the argument
// vector is deterministic. Without an explicit name a Podman-assigned one
// leaks with no attribution; without the labels no run-scoped sweep can find it.
func fixtureContainerIdentityArgs(name string, labels map[string]string) []string {
	args := []string{"--name", name}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "--label", key+"="+labels[key])
	}
	return args
}

// fixtureRunArgs is pure so tests can pin the run controls: always pull (so a
// digest launch is admitted by the registry at launch time), disable TLS
// verification only for the harness registry, keep the workload offline, and
// carry the explicit run-scoped name and cleanup labels.
func fixtureRunArgs(reference, name string, labels map[string]string) []string {
	args := []string{
		"run",
		"--rm",
		"--pull=always",
		"--tls-verify=false",
		"--network=none",
	}
	args = append(args, fixtureContainerIdentityArgs(name, labels)...)
	return append(args, reference)
}

// fixtureGitArgs is pure so tests can pin the Git probe: it runs only the local
// fixture build tag, never pulls (the base was resolved once), keeps the
// workload offline, replaces the marker entrypoint with git, and carries the
// explicit run-scoped name and cleanup labels.
func fixtureGitArgs(localTag, name string, labels map[string]string) []string {
	args := []string{
		"run",
		"--rm",
		"--pull=never",
		"--network=none",
	}
	args = append(args, fixtureContainerIdentityArgs(name, labels)...)
	return append(args, "--entrypoint", "git", localTag, "--version")
}

// GitVersion runs `git --version` inside one locally built fixture image and
// returns the reported line. It is the real proof that the fixture built from
// the digest-pinned SCM base carries the git binary the protected factory
// needs. Only this fixture's own local build namespace is accepted, and the
// probe never pulls.
func (builder *FixtureBuilder) GitVersion(ctx context.Context, localTag string) (string, error) {
	if !strings.HasPrefix(localTag, fixtureLocalTagPrefix) {
		return "", fmt.Errorf("refusing git probe in non-fixture image %q", localTag)
	}
	result, err := builder.command(ctx, builder.runLimit,
		fixtureGitArgs(localTag, fixtureProbeName("git"), runtimesupport.RunLabels())...)
	output := strings.TrimSpace(string(result.Stdout))
	if err != nil {
		return "", fmt.Errorf("run git in fixture %s: %w: %s", localTag, err, fixtureCommandTail(result))
	}
	if !strings.HasPrefix(output, "git version ") {
		return "", fmt.Errorf("fixture %s did not report a git version: %q", localTag, output)
	}
	return output, nil
}

// RunMarker runs one image reference through the real registry and returns the
// marker it printed. The reference must belong to the supplied harness
// registry: --tls-verify=false is confined to that host, so a later caller
// cannot silently pull a foreign image with TLS verification disabled.
func (builder *FixtureBuilder) RunMarker(ctx context.Context, instance *Registry, reference string) (string, error) {
	if instance == nil {
		return "", errors.New("fixture registry is nil")
	}
	host, err := fixtureReferenceHost(reference)
	if err != nil {
		return "", fmt.Errorf("fixture run reference: %w", err)
	}
	if want := instance.Host(); host != want {
		return "", fmt.Errorf("fixture run reference %q host %q is not the fixture registry host %q", reference, host, want)
	}
	// --pull=always materializes the reference locally: a tag becomes a local
	// tag and a digest reference becomes a local repository@sha256:... name
	// (displayed with a <none> tag). Both must be removed by exact name later.
	// Track it before the run can create it, so a failed run is still cleaned up.
	builder.trackCreated(reference)
	result, err := builder.command(ctx, builder.runLimit,
		fixtureRunArgs(reference, fixtureProbeName("run"), runtimesupport.RunLabels())...)
	stdout := strings.TrimSpace(string(result.Stdout))
	if err != nil {
		return "", fmt.Errorf("run fixture %s: %w: %s", reference, err, fixtureCommandTail(result))
	}
	if stdout == "" {
		return "", fmt.Errorf("fixture run %s printed no marker", reference)
	}
	return stdout, nil
}

// fixtureReferenceHost returns the registry host:port of a registry-qualified
// fixture run reference. It refuses a bare tag, a non-lowercase or malformed
// reference and a malformed digest: only the exact harness registry host may
// ever be passed to a run.
func fixtureReferenceHost(reference string) (string, error) {
	if reference == "" {
		return "", errors.New("fixture run reference is empty")
	}
	if strings.TrimSpace(reference) != reference {
		return "", fmt.Errorf("fixture run reference %q has surrounding whitespace", reference)
	}
	if strings.ToLower(reference) != reference {
		return "", fmt.Errorf("fixture run reference %q is not lowercase", reference)
	}
	if strings.ContainsAny(reference, " \t\r\n\\?") || strings.Contains(reference, "//") {
		return "", fmt.Errorf("fixture run reference %q is malformed", reference)
	}
	slash := strings.Index(reference, "/")
	if slash <= 0 || slash == len(reference)-1 {
		return "", fmt.Errorf("fixture run reference %q is not registry-qualified; it has no host and repository", reference)
	}
	host := reference[:slash]
	remainder := reference[slash+1:]
	if at := strings.Index(remainder, "@"); at >= 0 {
		if !validFixtureDigest(remainder[at+1:]) {
			return "", fmt.Errorf("fixture run reference %q has an invalid digest", reference)
		}
		remainder = remainder[:at]
	}
	if colon := strings.LastIndex(remainder, ":"); colon >= 0 {
		remainder = remainder[:colon]
	}
	if remainder == "" || strings.HasPrefix(remainder, "/") {
		return "", fmt.Errorf("fixture run reference %q has no repository", reference)
	}
	return host, nil
}

// trackCreated records one exact local image name that this fixture created or
// pulled. Names are recorded before the mutating command runs, so a failed
// build, push or pull is still cleaned up; cleanup tolerates a name that was
// never actually produced.
func (builder *FixtureBuilder) trackCreated(names ...string) {
	builder.mu.Lock()
	defer builder.mu.Unlock()
	if builder.created == nil {
		builder.created = make(map[string]struct{}, len(names))
	}
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if _, ok := builder.created[name]; ok {
			continue
		}
		builder.created[name] = struct{}{}
		builder.createdOrder = append(builder.createdOrder, name)
	}
}

// createdNames returns the tracked names in creation order. It is a snapshot:
// cleanup runs commands without holding the lock.
func (builder *FixtureBuilder) createdNames() []string {
	builder.mu.Lock()
	defer builder.mu.Unlock()
	return append([]string(nil), builder.createdOrder...)
}

// forgetCreated drops one name that is gone, so a later cleanup does not
// re-issue its removal.
func (builder *FixtureBuilder) forgetCreated(name string) {
	builder.mu.Lock()
	defer builder.mu.Unlock()
	if _, ok := builder.created[name]; !ok {
		return
	}
	delete(builder.created, name)
	for index, existing := range builder.createdOrder {
		if existing == name {
			builder.createdOrder = append(builder.createdOrder[:index], builder.createdOrder[index+1:]...)
			break
		}
	}
}

// Cleanup removes every exact local image name this builder created or pulled:
// the localhost build tags, the registry-qualified push tags (including a
// retargeted tag) and the names materialized by digest pulls. Removal is always
// by exact name, never by ID, never forced and never a prune, so shared
// content-addressed cache entries and a twin fixture's names survive.
func (builder *FixtureBuilder) Cleanup(ctx context.Context) error {
	var errs []error
	for _, name := range builder.createdNames() {
		gone, err := builder.removeFixtureName(ctx, name)
		if gone {
			builder.forgetCreated(name)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// removeFixtureName removes one exact fixture-owned name. The bool reports
// whether the name is now absent: removed, or already gone. A hard failure
// keeps the name tracked so the caller can retry it and must report it.
func (builder *FixtureBuilder) removeFixtureName(ctx context.Context, name string) (bool, error) {
	if !fixtureNameOwned(name) {
		return false, fmt.Errorf("refusing to remove non-fixture name %q", name)
	}
	result, err := builder.command(ctx, 2*time.Minute, "rmi", name)
	if err == nil {
		return true, nil
	}
	lowered := strings.ToLower(err.Error() + " " + string(result.Stderr))
	if strings.Contains(lowered, "no such image") || strings.Contains(lowered, "image not known") {
		return true, nil
	}
	return false, fmt.Errorf("remove fixture name %s: %w", name, err)
}

// fixtureNameOwned reports whether name is inside the fixture's own image
// namespace. Local build tags live under localhost/kodama-fixture-; the
// registry-qualified push and pull names live under <host>/kodama/fixture-.
// The check keeps cleanup away from a base image, a registry image or another
// fixture's names.
func fixtureNameOwned(name string) bool {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" || trimmed != name {
		return false
	}
	if strings.HasPrefix(trimmed, fixtureLocalTagPrefix) {
		return true
	}
	reference := trimmed
	if at := strings.LastIndex(reference, "@"); at >= 0 {
		reference = reference[:at]
	}
	if colon := strings.LastIndex(reference, ":"); colon > strings.LastIndex(reference, "/") {
		reference = reference[:colon]
	}
	parts := strings.Split(reference, "/")
	if len(parts) < 3 {
		return false
	}
	return parts[len(parts)-2] == "kodama" && strings.HasPrefix(parts[len(parts)-1], "fixture-")
}

// LocalImageNames lists local image names matching the exact reference prefix.
// It is read-only evidence for cleanup: after Cleanup, podman images with this
// fixture's unique prefix must return nothing.
func (builder *FixtureBuilder) LocalImageNames(ctx context.Context, prefix string) ([]string, error) {
	trimmed := strings.TrimSpace(prefix)
	if trimmed == "" {
		return nil, errors.New("fixture image prefix is empty")
	}
	result, err := builder.command(ctx, 2*time.Minute,
		"images", "--filter", "reference="+trimmed, "--format", "{{.Repository}}:{{.Tag}}")
	if err != nil {
		return nil, fmt.Errorf("list local images for %s: %w", trimmed, err)
	}
	var names []string
	for line := range strings.SplitSeq(string(result.Stdout), "\n") {
		if name := strings.TrimSpace(line); name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

// FixtureLocalTag is the unique local build tag for one shard and suffix.
func FixtureLocalTag(shardID, suffix string) string {
	return fixtureLocalTagPrefix + fixtureNameComponent(shardID) + ":" + fixtureNameComponent(suffix)
}

// FixtureLocalPrefix is the unique local image-name prefix for one shard. It is
// the exact reference filter a component test uses to prove no local build tag
// remains after cleanup.
func FixtureLocalPrefix(shardID string) string {
	return fixtureLocalTagPrefix + fixtureNameComponent(shardID)
}

// FixtureRepository is the unique registry repository for one shard and
// suffix.
func FixtureRepository(shardID, suffix string) string {
	return fixtureRepositoryRoot + fixtureNameComponent(shardID) + "-" + fixtureNameComponent(suffix)
}

// fixtureNameComponent keeps a generated repository or tag component inside
// the registry's lowercase character set.
func fixtureNameComponent(value string) string {
	var builder strings.Builder
	for _, r := range strings.ToLower(value) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_':
			builder.WriteRune(r)
		default:
			builder.WriteByte('-')
		}
	}
	out := strings.Trim(builder.String(), "-._")
	if out == "" {
		out = "fixture"
	}
	if len(out) > 80 {
		out = strings.TrimRight(out[:80], "-._")
	}
	return out
}

// fixtureCommandTail returns the last useful lines of a failed command's
// output without dumping an unbounded build log into a test failure.
func fixtureCommandTail(result harness.Result) string {
	text := strings.TrimSpace(string(result.Stderr))
	if text == "" {
		text = strings.TrimSpace(string(result.Stdout))
	}
	if text == "" {
		return "no output"
	}
	lines := strings.Split(text, "\n")
	if len(lines) > 8 {
		lines = lines[len(lines)-8:]
	}
	return strings.Join(lines, " | ")
}
