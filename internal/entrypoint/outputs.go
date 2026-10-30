package entrypoint

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"unicode/utf8"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
)

// Reason codes of failed results produced by the entrypoint.
const (
	ReasonExitNonzero          = "exit_nonzero"   // vocabulary of the contract fixtures
	ReasonCommandFailed        = "command_failed" // terminated by a signal without a timeout or interrupt
	ReasonCommandStartFailed   = "command_start_failed"
	ReasonCommandReportedFail  = "command_reported_failure"
	ReasonTimeout              = "timeout"
	ReasonInterrupted          = "interrupted"
	ReasonCheckoutFailed       = "checkout_failed"
	ReasonSecretUnavailable    = "secret_unavailable"
	ReasonArtifactUnavailable  = "artifact_unavailable"
	ReasonContextMismatch      = "context_mismatch"
	ReasonOutputMissing        = ReasonOutputInvalid // every declared-output violation is reported as output_invalid; the note names the cause
	ReasonOutputInvalid        = "output_invalid"
	ReasonOutputUploadFailed   = "output_upload_failed"
	ReasonManifestInvalid      = ReasonOutputInvalid
	ReasonSourceFailed         = "source_failed"
	ReasonSecretInSource       = "secret_in_source"
	ReasonBundleTooLarge       = "bundle_too_large"
	ReasonPublicationConflict  = "publication_conflict"
	ReasonPublicationFailed    = "publication_failed"
	ReasonPublicationUnsettled = "publication_unsettled"
	ReasonResultRejected       = "result_rejected"
)

// outputError is a declared-output problem that turns a zero exit into a
// failed result.
type outputError struct {
	reason string
	msg    string
}

func (e *outputError) Error() string { return e.msg }

func outputErr(reason, format string, args ...any) *outputError {
	return &outputError{reason: reason, msg: fmt.Sprintf(format, args...)}
}

// manifestDoc is the strict shape of /kodama/results.json.
type manifestDoc struct {
	Outcome  *string                    `json:"outcome"`
	Reason   *string                    `json:"reason"`
	Question *string                    `json:"question"`
	Outputs  map[string]json.RawMessage `json:"outputs"`
}

type ociValue struct {
	Digest      runtimev1.Digest `json:"digest"`
	RegistryRef string           `json:"registryRef"`
}

const maxManifestBytes = 1 << 20

var safeNameRe = regexp.MustCompile(runtimev1.Patterns["SafeName"])

// readManifest loads and strictly validates the manifest. A missing file is
// (nil, nil). Duplicate keys, floats, unknown top-level fields, a wrong
// outcome or an inconsistent question/reason are errors.
func readManifest(file string) (*manifestDoc, error) {
	fi, err := os.Lstat(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, outputErr(ReasonManifestInvalid, "manifest: %v", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, outputErr(ReasonManifestInvalid, "manifest is not a regular file")
	}
	if fi.Size() > maxManifestBytes {
		return nil, outputErr(ReasonManifestInvalid, "manifest larger than %d bytes", maxManifestBytes)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, outputErr(ReasonManifestInvalid, "manifest: %v", err)
	}
	canonical, err := runtimev1.Canonicalize(raw)
	if err != nil {
		return nil, outputErr(ReasonManifestInvalid, "manifest: %v", err)
	}
	var m manifestDoc
	dec := json.NewDecoder(bytes.NewReader(canonical))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, outputErr(ReasonManifestInvalid, "manifest: %v", err)
	}
	if m.Outcome == nil {
		return nil, outputErr(ReasonManifestInvalid, "manifest: outcome is required")
	}
	switch runtimev1.Outcome(*m.Outcome) {
	case runtimev1.OutcomeCompleted:
		if m.Reason != nil || m.Question != nil {
			return nil, outputErr(ReasonManifestInvalid, "manifest: completed takes no reason or question")
		}
	case runtimev1.OutcomeFailed:
		if m.Question != nil || len(m.Outputs) != 0 {
			return nil, outputErr(ReasonManifestInvalid, "manifest: failed takes no question or outputs")
		}
	case runtimev1.OutcomeNeedsInput:
		if m.Question == nil || strings.TrimSpace(*m.Question) == "" || utf8.RuneCountInString(*m.Question) > 4096 {
			return nil, outputErr(ReasonManifestInvalid, "manifest: needs_input requires a non-blank question of at most 4096 characters")
		}
		if m.Reason != nil || len(m.Outputs) != 0 {
			return nil, outputErr(ReasonManifestInvalid, "manifest: needs_input takes no reason or outputs")
		}
	default:
		return nil, outputErr(ReasonManifestInvalid, "manifest: unknown outcome %q", *m.Outcome)
	}
	return &m, nil
}

// scalarValue decodes a manifest output for a string, boolean or integer
// declaration.
func scalarValue(name string, t runtimev1.OutputType, raw json.RawMessage) (runtimev1.Value, error) {
	var v runtimev1.Value
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, outputErr(ReasonOutputInvalid, "output %q: %v", name, err)
	}
	ok := (t == runtimev1.OutputString && v.IsString()) ||
		(t == runtimev1.OutputBoolean && v.IsBool()) ||
		(t == runtimev1.OutputInteger && v.IsInteger())
	if !ok {
		return v, outputErr(ReasonOutputInvalid, "output %q must be of type %s", name, t)
	}
	if err := v.Validate(); err != nil {
		return v, outputErr(ReasonOutputInvalid, "output %q: %v", name, err)
	}
	return v, nil
}

func ociValueOf(name string, raw json.RawMessage) (ociValue, error) {
	var o ociValue
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&o); err != nil {
		return o, outputErr(ReasonOutputInvalid, "output %q must be {\"digest\",\"registryRef\"}: %v", name, err)
	}
	probe := runtimev1.ArtifactReserveRequest{Name: name, Kind: runtimev1.KindOCI, Role: runtimev1.RoleOutput, Digest: o.Digest, Registry: &o.RegistryRef}
	if err := probe.Validate(); err != nil {
		return o, outputErr(ReasonOutputInvalid, "output %q: %v", name, err)
	}
	return o, nil
}

// fileOutput is a validated, ready-to-upload declared file.
type fileOutput struct {
	name      string // output declaration name
	path      string
	file      *os.File
	size      int64
	digest    runtimev1.Digest
	mediaType string
	artifact  string // artifact name
}

// openOutputFile resolves a declared /kodama/outputs/... path under root and
// opens it. Every component is checked with Lstat: symlinks are refused
// anywhere below the root, the leaf must be a regular file with a single link,
// and the opened descriptor must be the same file. The result is size-checked
// by the caller.
func openOutputFile(root, declared string) (*os.File, os.FileInfo, error) {
	if !strings.HasPrefix(declared, OutputsPrefix+"/") {
		return nil, nil, outputErr(ReasonOutputInvalid, "declared path %q is outside %s", declared, OutputsPrefix)
	}
	rel := strings.TrimPrefix(declared, OutputsPrefix+"/")
	segments := strings.Split(rel, "/")
	cur, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, nil, outputErr(ReasonOutputMissing, "outputs root: %v", err)
	}
	for i, seg := range segments {
		if seg == "" || seg == "." || seg == ".." {
			return nil, nil, outputErr(ReasonOutputInvalid, "declared path %q is not normalized", declared)
		}
		cur = filepath.Join(cur, seg)
		fi, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, outputErr(ReasonOutputMissing, "declared output %q was not produced", declared)
		}
		if err != nil {
			return nil, nil, outputErr(ReasonOutputInvalid, "declared output %q: %v", declared, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil, nil, outputErr(ReasonOutputInvalid, "declared output %q passes through a symbolic link", declared)
		}
		last := i == len(segments)-1
		if !last && !fi.IsDir() {
			return nil, nil, outputErr(ReasonOutputInvalid, "declared output %q: parent is not a directory", declared)
		}
		if last {
			if !fi.Mode().IsRegular() {
				return nil, nil, outputErr(ReasonOutputInvalid, "declared output %q is not a regular file", declared)
			}
			if st, ok := fi.Sys().(*syscall.Stat_t); ok && uint64(st.Nlink) > 1 {
				return nil, nil, outputErr(ReasonOutputInvalid, "declared output %q has multiple hard links", declared)
			}
			f, err := os.OpenFile(cur, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
			if err != nil {
				return nil, nil, outputErr(ReasonOutputInvalid, "declared output %q: %v", declared, err)
			}
			ofi, err := f.Stat()
			if err != nil || !os.SameFile(fi, ofi) || !ofi.Mode().IsRegular() {
				f.Close()
				return nil, nil, outputErr(ReasonOutputInvalid, "declared output %q changed while opening", declared)
			}
			return f, ofi, nil
		}
	}
	return nil, nil, outputErr(ReasonOutputInvalid, "declared output %q is empty", declared)
}

var mediaTypes = map[string]string{
	".json": "application/json", ".txt": "text/plain", ".log": "text/plain", ".md": "text/markdown",
	".xml": "application/xml", ".html": "text/html", ".csv": "text/csv", ".yaml": "application/yaml", ".yml": "application/yaml",
}

func mediaTypeFor(name string) string {
	if t, ok := mediaTypes[strings.ToLower(path.Ext(name))]; ok {
		return t
	}
	return "application/octet-stream"
}

// hashFile returns the digest of the whole file.
func hashFile(f *os.File, size int64) (runtimev1.Digest, error) {
	h := sha256.New()
	n, err := io.Copy(h, io.NewSectionReader(f, 0, size))
	if err != nil {
		return "", err
	}
	if n != size {
		return "", errors.New("file changed while hashing")
	}
	return runtimev1.Digest("sha256:" + hex.EncodeToString(h.Sum(nil))), nil
}

// artifactName returns the artifact name of a declared file output: the
// declared output name, never the file's base name, so the file may sit
// anywhere under the outputs root. A name that is not a SafeName or that
// another output has already reserved is refused.
func artifactName(output string, taken map[string]bool) (string, error) {
	if !safeNameRe.MatchString(output) {
		return "", outputErr(ReasonOutputInvalid, "output %q is not a valid artifact name", output)
	}
	if taken[output] {
		return "", outputErr(ReasonOutputInvalid, "output %q is declared more than once", output)
	}
	taken[output] = true
	return output, nil
}
