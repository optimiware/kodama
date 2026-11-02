package pipeline

import (
	"regexp"

	pipelinev1alpha2 "github.com/optimiware/kodama/contracts/pipeline/v1alpha2"
	pipelinev1alpha3 "github.com/optimiware/kodama/contracts/pipeline/v1alpha3"
)

// runSharedVersion returns the apiVersion of a generic tree that declares a
// run_shared format (kodama/v1alpha2 or kodama/v1alpha3) and "" for anything
// else. Anything else is judged by the kodama/v1alpha1 schema, which rejects
// unknown versions as before.
func runSharedVersion(tree any) string {
	m, ok := tree.(map[string]any)
	if !ok {
		return ""
	}
	if v, _ := m["apiVersion"].(string); v == pipelinev1alpha2.APIVersion || v == pipelinev1alpha3.APIVersion {
		return v
	}
	return ""
}

// isRunSharedTree reports whether a generic tree declares a run_shared format.
func isRunSharedTree(tree any) bool { return runSharedVersion(tree) != "" }

const svcPrefix = `^\$\.spec\.services(?:\.[A-Za-z0-9_-]+|\[[^\]]*\])`

// reNameKeyed matches the paths of the objects whose keys are names.
var reNameKeyed = regexp.MustCompile(`^\$\.spec\.(?:inputs|services)$|` + svcPrefix + `\.(?:ports|env|secretFiles)$|^\$\.spec\.steps\[\d+\]\.(?:env|secretFiles|outputs)$`)

var (
	reReadiness    = regexp.MustCompile(svcPrefix + `\.readiness(?:\.|\[|$)`)
	reResources    = regexp.MustCompile(svcPrefix + `\.resources(?:\.|$)`)
	reStorage      = regexp.MustCompile(svcPrefix + `\.storage(?:\.|\[|$)`)
	reRole         = regexp.MustCompile(svcPrefix + `\.role$`)
	reEnvOrFiles   = regexp.MustCompile(svcPrefix + `\.(?:env|secretFiles)$`)
	reEgress       = regexp.MustCompile(`(?:` + svcPrefix + `|^\$\.spec\.steps\[\d+\])\.egress(?:\[|$)`)
	reEdges        = regexp.MustCompile(`^\$\.spec\.network(?:\.edges|$)`)
	reTimeout      = regexp.MustCompile(`^\$\.spec\.(?:timeoutSeconds|steps\[\d+\]\.timeoutSeconds)$`)
	reRunWorkspace = regexp.MustCompile(`^\$\.spec\.runWorkspace\.sizeMiB$`)
	reSourcePath   = regexp.MustCompile(`^\$\.spec\.sources\[\d+\]\.path$`)
)

// v2SchemaCode maps a schema keyword failure at a kodama/v1alpha2 document path
// to the contract's specific diagnostic code. It applies only to documents of
// that version and only to value-level keywords: type errors, missing fields
// and unknown fields keep their generic codes. The Go shape checks emit the
// same codes for the same rule, so a rule has one code on every path.
func v2SchemaCode(v2 bool, path, kw string) (string, bool) {
	if !v2 || kw == "type" {
		return "", false
	}
	switch {
	case reReadiness.MatchString(path):
		return CodeInvalidProbe, true
	case reResources.MatchString(path), reRunWorkspace.MatchString(path):
		return CodeInvalidResources, true
	case reStorage.MatchString(path):
		return CodeInvalidStorage, true
	case reEdges.MatchString(path):
		return CodeInvalidEdge, true
	case reEgress.MatchString(path):
		return CodeInvalidEgress, true
	case reRole.MatchString(path):
		return CodeInvalidRole, true
	case reSourcePath.MatchString(path):
		return CodeInvalidSourcePath, true
	case path == "$.spec.sources" && kw == "maxItems":
		return CodeLimitExceeded, true
	case reTimeout.MatchString(path) && (kw == "minimum" || kw == "maximum"):
		return CodeInvalidTimeout, true
	case (path == "$.spec.services" || reEnvOrFiles.MatchString(path)) && kw == "maxProperties":
		return CodeLimitExceeded, true
	}
	return "", false
}

// v2Message is the constant message of a code produced by v2SchemaCode.
func v2Message(code string) string {
	switch code {
	case CodeInvalidProbe:
		return "the readiness declaration is outside its allowed shape or range"
	case CodeInvalidResources:
		return "the resource value is outside its allowed range"
	case CodeInvalidStorage:
		return "the storage declaration is outside its allowed shape or range"
	case CodeInvalidEdge:
		return "the network edge declaration is outside its allowed shape or range"
	case CodeInvalidEgress:
		return "an egress entry names a profile; it is never a host, CIDR or port"
	case CodeInvalidRole:
		return "the service role must be general or builder"
	case CodeInvalidSourcePath:
		return "a source path is relative to /workspace, clean and made of plain components"
	case CodeInvalidTimeout:
		return "the timeout is outside its allowed range"
	}
	return "the declaration exceeds its allowed size"
}
