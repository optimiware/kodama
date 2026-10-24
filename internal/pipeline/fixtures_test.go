package pipeline_test

import (
	"encoding/json"
	"testing"

	"github.com/optimiware/kodama/internal/pipeline"
)

// Test_contract_cases runs the shared contract fixtures through the one
// validator that the CLI and API both use, so the published cases and the
// executable rules cannot drift. PX-01, PX-02, PX-14, AC-12.
func Test_contract_cases(t *testing.T) {
	var cases []struct {
		Name     string `json:"name"`
		Format   string `json:"format"`
		Code     string `json:"code"`
		Document string `json:"document"`
	}
	if err := json.Unmarshal(readFixture(t, "cases.json"), &cases); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t)
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			_, err := e.validate(c.Document)
			expectCode(t, err, c.Code)
		})
	}
	t.Run("planned M2 shapes are valid but not enabled under M1 capabilities", func(t *testing.T) {
		src := string(readFixture(t, "valid/m2-agent-gate.yaml"))
		e.mustValidate(t, src)
		m1 := newEnv(t, pipeline.KindContainer)
		_, err := m1.validate(src)
		expectCode(t, err, pipeline.CodeKindDisabled)
	})
}
