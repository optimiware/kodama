package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Pinned official images. These are the single source of truth for component
// tests; no test may invent a different tag. Every tag is a fixed
// major.minor(-variant), never "latest".
const (
	ImagePostgres = "docker.io/library/postgres:18-alpine"
	ImageNATS     = "docker.io/library/nats:2.15-alpine"
	ImageRegistry = "docker.io/library/registry:3.1"
)

// ImageRecord is one recorded tag/digest pair.
type ImageRecord struct {
	Tag        string    `json:"tag"`
	Digest     string    `json:"digest"`
	ImageID    string    `json:"image_id"`
	RecordedAt time.Time `json:"recorded_at"`
}

// Pull pulls tag. A pull failure is reported as ErrImageUnavailable so callers
// can pause or skip explicitly instead of pretending the image is usable.
func (p *Podman) Pull(ctx context.Context, tag string) error {
	if strings.TrimSpace(tag) == "" {
		return fmt.Errorf("%w: empty image tag", ErrImageUnavailable)
	}
	if _, err := p.run(ctx, p.pullLimit, "pull", tag); err != nil {
		return fmt.Errorf("%w: pull %s: %v", ErrImageUnavailable, tag, err)
	}
	return nil
}

// ImageDigest returns the image manifest digest recorded by Podman. It never
// pulls; a missing image is an ordinary error (not ErrImageUnavailable) so
// EnsureImage can distinguish absence from an unavailable registry.
func (p *Podman) ImageDigest(ctx context.Context, tag string) (string, error) {
	if strings.TrimSpace(tag) == "" {
		return "", fmt.Errorf("harness: empty image tag")
	}
	res, err := p.run(ctx, p.operationLimit, "image", "inspect", "--format", "{{.Digest}}", tag)
	if err != nil {
		return "", fmt.Errorf("harness: inspect image %s: %w", tag, err)
	}
	digest := strings.TrimSpace(string(res.Stdout))
	if digest == "" || digest == "<none>" {
		return "", fmt.Errorf("harness: image %s has no manifest digest", tag)
	}
	if !strings.HasPrefix(digest, "sha256:") {
		return "", fmt.Errorf("harness: image %s returned unexpected digest %q", tag, digest)
	}
	return digest, nil
}

// ImageID returns the local image ID for tag.
func (p *Podman) ImageID(ctx context.Context, tag string) (string, error) {
	res, err := p.run(ctx, p.operationLimit, "image", "inspect", "--format", "{{.Id}}", tag)
	if err != nil {
		return "", fmt.Errorf("harness: inspect image id %s: %w", tag, err)
	}
	id := strings.TrimSpace(string(res.Stdout))
	if id == "" {
		return "", fmt.Errorf("harness: image %s has no id", tag)
	}
	return id, nil
}

// EnsureImage returns tag's digest, pulling the image when it is absent. The
// tag/digest pair is recorded into the test output and, when configured, into
// <evidence>/images.jsonl. A failed pull returns ErrImageUnavailable.
func (p *Podman) EnsureImage(ctx context.Context, tag string) (string, error) {
	record, err := p.ensureImageRecord(ctx, tag)
	if err != nil {
		return "", err
	}
	return record.Digest, nil
}

// ensureImageRecord resolves tag once and returns the complete recorded
// identity. Callers that retain the selection use the same record they
// recorded, so the evidence cannot describe a different image than the one
// they launch.
func (p *Podman) ensureImageRecord(ctx context.Context, tag string) (ImageRecord, error) {
	digest, err := p.ImageDigest(ctx, tag)
	if err != nil {
		if pullErr := p.Pull(ctx, tag); pullErr != nil {
			return ImageRecord{}, pullErr
		}
		digest, err = p.ImageDigest(ctx, tag)
		if err != nil {
			return ImageRecord{}, fmt.Errorf("harness: image %s is still unusable after pull: %w", tag, err)
		}
	}
	imageID, idErr := p.ImageID(ctx, tag)
	if idErr != nil {
		// The digest is the required evidence; a missing ID is not fatal.
		p.logf("harness: image %s: %v", tag, idErr)
		imageID = ""
	}
	record := ImageRecord{
		Tag:        tag,
		Digest:     digest,
		ImageID:    imageID,
		RecordedAt: p.clock.Now().UTC(),
	}
	if err := p.recordImage(record); err != nil {
		return ImageRecord{}, err
	}
	return record, nil
}

// recordImage logs the record and appends it to the JSON evidence file when
// KODAMA_EVIDENCE_DIR (or WithEvidenceDir) is set.
func (p *Podman) recordImage(rec ImageRecord) error {
	p.logf("harness: image %s digest %s id %s", rec.Tag, rec.Digest, rec.ImageID)
	if p.evidenceDir == "" {
		return nil
	}
	if err := os.MkdirAll(p.evidenceDir, 0o755); err != nil {
		return fmt.Errorf("harness: create evidence dir: %w", err)
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("harness: encode image evidence: %w", err)
	}
	line = append(line, '\n')
	path := filepath.Join(p.evidenceDir, "images.jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("harness: open image evidence: %w", err)
	}
	defer file.Close()
	if _, err := file.Write(line); err != nil {
		return fmt.Errorf("harness: write image evidence: %w", err)
	}
	return nil
}
