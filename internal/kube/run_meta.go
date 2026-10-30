package kube

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

// Object keys of the run-resource adapter. Labels select objects; they are never
// authority. Every adoption, stop and removal verifies the annotations below
// against the committed handle and spec, and the bound instance UID.
const (
	// LabelResourceOwner marks an object the run-resource adapter owns.
	LabelResourceOwner = "kodama.optimi.com/resource-owner"
	ValueResourceOwner = "run-resources"

	// LabelRunKey is the run's label value: a hash, because identifiers do not
	// fit label syntax. LabelResourceKind is the resource kind, LabelRole the
	// workload role (step, preparation or service), LabelWorkload a hash of the
	// workload key within the run and LabelRunShared marks every Pod of a
	// run_shared run (the clients of the tenant model).
	LabelRunKey       = "kodama.optimi.com/run-key"
	LabelResourceKind = "kodama.optimi.com/resource-kind"
	LabelRole         = "kodama.optimi.com/run-role"
	LabelWorkload     = "kodama.optimi.com/workload-key"
	LabelRunShared    = "kodama.optimi.com/run-shared"

	RoleLabelStep        = "step"
	RoleLabelPreparation = "preparation"
	RoleLabelService     = "service"

	AnnResourceKind = "kodama.optimi.com/resource-kind"
	AnnResourceKey  = "kodama.optimi.com/resource-key"
	AnnResourceName = "kodama.optimi.com/resource-name"
	// AnnResourceUID is the instance UID of the resource's record: the identity
	// of a resource that has no workload object (the network).
	AnnResourceUID = "kodama.optimi.com/resource-uid"
	AnnPolicy      = "kodama.optimi.com/policy"
	AnnRecord      = "kodama.optimi.com/resource-record"
)

// framedHex is the lowercase hex SHA-256 of the parts, each framed by its byte
// length as a 4-byte big-endian integer: the framing of the resource names and
// endpoint hosts (execution.ResourceName, execution.EndpointHost).
func framedHex(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(p)))
		h.Write(n[:])
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// RunKey is the label value that selects one run's Pods: the first 32 hex
// characters of the framed digest of (tenantId, projectId, runId). Its first 16
// characters are the run part of every endpoint host.
func RunKey(scope domain.Scope, run domain.RunID) string {
	return framedHex(string(scope.TenantID), string(scope.ProjectID), string(run))[:32]
}

// WorkloadHash is the label value of one workload of a run: the first 24 hex
// characters of the framed digest of (run key, workload key). The workload key is
// a step ID, "preparation" or "service.NAME".
func WorkloadHash(scope domain.Scope, run domain.RunID, workload string) string {
	return framedHex(RunKey(scope, run), workload)[:24]
}

// ownerLabels are the labels every run-resource object carries.
func ownerLabels(scope domain.Scope, run domain.RunID, kind execution.ResourceKind) map[string]string {
	return map[string]string{LabelResourceOwner: ValueResourceOwner, LabelRunKey: RunKey(scope, run), LabelResourceKind: string(kind)}
}
