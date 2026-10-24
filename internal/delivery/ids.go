package delivery

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"

	"github.com/optimiware/kodama/internal/domain"
)

// DeploymentID identifies a deployment record. Like every identifier it is
// opaque; ownership is never inferred from it.
type DeploymentID string

var idPattern = regexp.MustCompile(domain.IDPattern)

// Validate checks the identifier syntax.
func (id DeploymentID) Validate() error {
	if !idPattern.MatchString(string(id)) {
		return invalidf("deployment id must match %s", domain.IDPattern)
	}
	return nil
}

// IDs generates opaque identifiers.
type IDs interface {
	NewID(prefix string) (string, error)
}

// RandomIDs generates identifiers from 128 random bits.
type RandomIDs struct{}

// NewID implements IDs.
func (RandomIDs) NewID(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("delivery: generate ID: %w", err)
	}
	return prefix + "_" + hex.EncodeToString(b[:]), nil
}

// DeploymentIDFor derives the deployment identity of a logical action. One
// deployment exists per (tenant, project, action key), so its identity can be
// known before the record commits: a retry after an unknown commit outcome, a
// concurrent identical registration and an artifact-retention pin taken in
// advance all name the same deployment, and none can create a replacement.
func DeploymentIDFor(scope domain.Scope, actionKey string) DeploymentID {
	h := sha256.New()
	for _, part := range []string{"kodama/deployment/v1", string(scope.TenantID), string(scope.ProjectID), actionKey} {
		fmt.Fprintf(h, "%d:%s|", len(part), part)
	}
	return DeploymentID("dep_" + hex.EncodeToString(h.Sum(nil)[:16]))
}
