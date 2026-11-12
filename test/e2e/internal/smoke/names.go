//go:build e2e

package smoke

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Scenario is one release-smoke scenario. The value is used in evidence and in
// run-unique markers/keys, never in a product branch.
type Scenario string

const (
	// RequestWait is S1: a planned request answered by the operator.
	RequestWait Scenario = "RequestWait"
	// OperatorRetry is S4: an operator-only retry of a failed task.
	OperatorRetry Scenario = "OperatorRetry"
	// ConversationHandoff is S3: an ask/reply handoff into a fresh execution.
	ConversationHandoff Scenario = "ConversationHandoff"
	// ProtectedTask is S2: external harness, test, human review and protected
	// Git integration in one execution and workspace.
	ProtectedTask Scenario = "ProtectedTask"
	// HumanPush is S5: a human push starts a pipeline subscribed to its ref.
	HumanPush Scenario = "HumanPush"
	// Delivery is the digest-pinned delivery-evidence scenario.
	Delivery Scenario = "Delivery"
)

// Names are the run-unique values one scenario writes into pipeline markers,
// idempotency keys, answers and durable references. They are lower-case and
// bounded, so they are safe in shell scripts and receipt keys, and a leftover
// run is attributable from its identity alone.
type Names struct {
	Run      string
	Scenario Scenario
}

// NewNames binds one run identity to one scenario.
func NewNames(run string, scenario Scenario) Names {
	return Names{Run: run, Scenario: scenario}
}

// slug is the lower-case scenario identity used in names.
func (names Names) slug() string { return strings.ToLower(string(names.Scenario)) }

// Prefix is the stable, scenario-unique name stem.
func (names Names) Prefix() string { return "w49-" + names.slug() + "-" + names.Run }

// Key returns one run-unique idempotency key.
func (names Names) Key(step string) string { return names.Prefix() + "-key-" + step }

// Marker returns one run-unique positive log marker.
func (names Names) Marker(label string) string { return names.Prefix() + "-" + label }

// Answer returns the scenario's run-unique reviewer/operator answer.
func (names Names) Answer() string { return names.Prefix() + "-answer" }

// WorkRef returns one scenario-unique granted work branch. Work refs are the
// only branches the default policy grants to an engineer or an execution.
func (names Names) WorkRef(label string) string {
	return "refs/heads/work/" + names.Prefix() + "-" + label
}

// PipelineName returns one scenario-unique stored pipeline name.
func (names Names) PipelineName(label string) string {
	return names.Prefix() + "-" + label
}

// Reference returns an ordinary artifact-digest durable reference. The product
// treats references as opaque restoration pointers with no character grammar,
// so this needs no Git. The Git scenarios use a real pushed-commit reference
// instead.
func (names Names) Reference() string {
	sum := sha256.Sum256([]byte(names.Prefix() + "-reference"))
	return "sha256:" + hex.EncodeToString(sum[:])
}
