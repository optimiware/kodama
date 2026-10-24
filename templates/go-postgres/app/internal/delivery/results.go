package delivery

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

var reasonRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// WriteResults writes the entrypoint's command result manifest atomically.
// Declared file artifacts are written under the outputs root and are never
// listed here; scalar outputs and OCI artifacts are. outcome is "completed" or
// "failed"; a failure carries a reason code and no outputs.
func WriteResults(path, outcome, reason string, outputs map[string]any) error {
	if path == "" {
		return errors.New("delivery: the results file path is required (KODAMA_RESULTS_FILE)")
	}
	doc := map[string]any{"outcome": outcome}
	switch outcome {
	case "completed":
		if len(outputs) > 0 {
			doc["outputs"] = outputs
		}
	case "failed":
		if !reasonRE.MatchString(reason) {
			return fmt.Errorf("delivery: %q is not a valid failure reason code", reason)
		}
		doc["reason"] = reason
	default:
		return fmt.Errorf("delivery: unsupported outcome %q", outcome)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	return atomicWrite(path, raw)
}

func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// WriteArtifact writes a declared file artifact under the outputs root. The name
// must be a plain file name: nothing can escape the root.
func WriteArtifact(outputsDir, name string, v any) error {
	if outputsDir == "" || name == "" || filepath.Base(name) != name || name == "." || name == ".." {
		return errors.New("delivery: invalid artifact destination")
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(outputsDir, name), append(raw, '\n'))
}
