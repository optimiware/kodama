package execution

import (
	"errors"
	"strings"
	"testing"

	pipelinev1 "github.com/optimiware/kodama/contracts/pipeline/v1"
)

func Test_unit_pipeline_version_guard(t *testing.T) {
	if err := checkDefinitionVersion(pipelinev1.APIVersion); err != nil {
		t.Fatalf("current version refused: %v", err)
	}
	const secret = "kodama/v2-SECRET-token"
	for name, version := range map[string]string{
		"empty":   "",
		"future":  "kodama/v2",
		"unknown": secret,
		"case":    strings.ToUpper(pipelinev1.APIVersion),
		"padded":  " " + pipelinev1.APIVersion,
	} {
		err := checkDefinitionVersion(version)
		if !errors.Is(err, ErrAdmissionRefused) || !IsBusinessError(err) {
			t.Errorf("%s: want classified refusal, got %v", name, err)
			continue
		}
		if version != "" && strings.Contains(err.Error(), strings.TrimSpace(version)) {
			t.Errorf("%s: error leaks the document version: %v", name, err)
		}
	}
}
