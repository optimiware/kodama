package pipeline

import "regexp"

// isV1Alpha2Tree reports whether a generic tree declares the kodama/v1alpha2
// apiVersion. Anything else is judged by the kodama/v1alpha1 schema, which
// rejects unknown versions as before.
func isV1Alpha2Tree(tree any) bool {
	m, ok := tree.(map[string]any)
	return ok && m["apiVersion"] == "kodama/v1alpha2"
}

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
	case CodeInvalidTimeout:
		return "the timeout is outside its allowed range"
	}
	return "the declaration exceeds its allowed size"
}
