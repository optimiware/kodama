package memory_test

// Agent pins in shared (v2) and source-set (v3) contexts (T-163). The execution
// module consults a consumer-owned port while it builds the context of an
// attempt, so the profile and design baseline are part of the frozen context and
// its digest. The port here is a table double keyed by a marker argument; the
// real implementation (internal/agents/adapters.Pinner) is exercised end to end in
// that package. A step without the marker is not touched.

import (
	"bytes"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
)

const (
	pinMarker = "fake.agent/v1"
	badMarker = "fake.agent/bad"
)

var fakePin = func() execution.AgentPin {
	b, err := runtimev1.NewDesignBaseline([]string{"AR-01"}, []string{}, []runtimev1.SourceRef{
		{Repository: "https://git.example.invalid/design.git", Revision: runtimev1.GitCommit(strings.Repeat("c", 40)), Path: "docs"},
	}, []string{})
	if err != nil {
		panic(err)
	}
	return execution.AgentPin{Profile: runtimev1.Profile{Name: "coding", Version: 3, WrapperVersion: "1.0.0"}, DesignBaseline: b}
}()

// pinTable pins a command whose first argument is the marker.
type pinTable struct {
	mu    sync.Mutex
	calls []domain.TenantID
}

func (p *pinTable) PinAgent(tenant domain.TenantID, c runtimev1.Command) (execution.AgentPin, bool, error) {
	p.mu.Lock()
	p.calls = append(p.calls, tenant)
	p.mu.Unlock()
	if len(c.Args) == 0 {
		return execution.AgentPin{}, false, nil
	}
	switch c.Args[0] {
	case pinMarker:
		return fakePin, true, nil
	case badMarker:
		return execution.AgentPin{}, false, execution.Invalidf("the frozen agent payload is invalid")
	}
	return execution.AgentPin{}, false, nil
}

func (p *pinTable) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

func pinnedStep(marker string) string {
	return executiontest.SharedStep("job", "args: ["+marker+"]")
}

// v2Claim claims the only attempt of a shared run of one container step.
func v2Claim(t *testing.T, pins execution.AgentPins, step string) *execution.AttemptSpec {
	t.Helper()
	_, s := executiontest.NewSharedMem(t, func(mo *executiontest.MemOptions) { mo.AgentPins = pins })
	r := executiontest.NewSharedRun(t, s, executiontest.SharedSpecDoc(false, step))
	r.Up()
	in, err := r.Claim()
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	return in
}

// v3Claim is v2Claim for a source-set run. The repository name is fixed so that
// two fixtures freeze byte-identical contexts.
func v3Claim(t *testing.T, pins execution.AgentPins, step string) (*executiontest.SourceRun, *execution.AttemptSpec) {
	t.Helper()
	_, s := executiontest.NewSourceMem(t, executiontest.SourceOptions{AgentPins: pins})
	r := executiontest.NewSourceRunSteps(t, s, step)
	return r, r.Claim()
}

func Test_unit_shared_context_carries_agent_pins(t *testing.T) {
	t.Run("a v2 attempt of an agent step freezes the pins into its context and digest", func(t *testing.T) {
		pins := &pinTable{}
		in := v2Claim(t, pins, pinnedStep(pinMarker))
		c, err := in.DecodeContextV2()
		if err != nil {
			t.Fatal(err)
		}
		if c.Profile == nil || *c.Profile != fakePin.Profile || c.DesignBaseline == nil || !reflect.DeepEqual(*c.DesignBaseline, fakePin.DesignBaseline) {
			t.Fatalf("pins: %+v %+v", c.Profile, c.DesignBaseline)
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("the pinned context violates the runtime v2 contract: %v", err)
		}
		if pins.count() == 0 {
			t.Fatal("the port was not consulted")
		}
		bare := v2Claim(t, nil, pinnedStep(pinMarker))
		if bare.ContextDigest == in.ContextDigest || bytes.Equal(bare.Context, in.Context) {
			t.Fatal("the pins are not part of the frozen context and its digest")
		}
	})

	t.Run("a v3 attempt of an agent step freezes the pins into its context and digest", func(t *testing.T) {
		pins := &pinTable{}
		_, in := v3Claim(t, pins, pinnedStep(pinMarker))
		c, err := in.DecodeContextV3()
		if err != nil {
			t.Fatal(err)
		}
		if c.Profile == nil || *c.Profile != fakePin.Profile || c.DesignBaseline == nil || !reflect.DeepEqual(*c.DesignBaseline, fakePin.DesignBaseline) {
			t.Fatalf("pins: %+v %+v", c.Profile, c.DesignBaseline)
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("the pinned context violates the runtime v3 contract: %v", err)
		}
		if pins.count() == 0 {
			t.Fatal("the port was not consulted")
		}
		// The tenant is the run's, never a caller's claim.
		for _, tenant := range pins.calls {
			if tenant != executiontest.SharedScope.TenantID {
				t.Fatalf("the port saw tenant %q", tenant)
			}
		}
	})

	t.Run("an invalid payload fails closed: no attempt is created", func(t *testing.T) {
		_, s := executiontest.NewSharedMem(t, func(mo *executiontest.MemOptions) { mo.AgentPins = &pinTable{} })
		r := executiontest.NewSharedRun(t, s, executiontest.SharedSpecDoc(false, pinnedStep(badMarker)))
		r.Up()
		in, err := r.Claim()
		if err == nil || !errors.Is(err, execution.ErrInvalid) || in != nil {
			t.Fatalf("an invalid payload must refuse the claim: %v %v", in, err)
		}
		if n := len(r.Get().Attempts); n != 0 {
			t.Fatalf("%d attempts were recorded", n)
		}
	})
}

func Test_unit_shared_context_without_agent_payload_unchanged(t *testing.T) {
	plain := executiontest.SharedStep("job")

	t.Run("v2: a configured port leaves a step without a payload byte-identical", func(t *testing.T) {
		pins := &pinTable{}
		with := v2Claim(t, pins, plain)
		without := v2Claim(t, nil, plain)
		if pins.count() == 0 {
			t.Fatal("the port was not consulted")
		}
		if with.ContextDigest != without.ContextDigest || !bytes.Equal(with.Context, without.Context) {
			t.Fatalf("the context of a step without a payload changed:\n%s\n%s", with.Context, without.Context)
		}
		c, err := runtimev2.DecodeRequest[runtimev2.ContextV2](with.Context)
		if err != nil || c.Profile != nil || c.DesignBaseline != nil {
			t.Fatalf("%+v %v", c, err)
		}
		if !bytes.Contains(with.Context, []byte(`"designBaseline":null`)) || !bytes.Contains(with.Context, []byte(`"profile":null`)) {
			t.Fatal("the members stay present and null, as before")
		}
	})

	t.Run("v3: a configured port leaves a step without a payload byte-identical", func(t *testing.T) {
		pins := &pinTable{}
		_, with := v3Claim(t, pins, plain)
		_, without := v3Claim(t, nil, plain)
		if pins.count() == 0 {
			t.Fatal("the port was not consulted")
		}
		// The two fixtures differ only in their (global sequence) repository name,
		// which the repository ID, URL and the source set digest derive from.
		norm := func(in *execution.AttemptSpec) string {
			out := sequenced.ReplaceAllString(string(in.Context), "REPO")
			return digested.ReplaceAllString(out, `"sourceSetDigest":"MASKED"`)
		}
		if norm(with) != norm(without) {
			t.Fatalf("the context of a step without a payload changed:\n%s\n%s", with.Context, without.Context)
		}
		c, err := runtimev3.DecodeRequest[runtimev3.ContextV3](with.Context)
		if err != nil || c.Profile != nil || c.DesignBaseline != nil {
			t.Fatalf("%+v %v", c, err)
		}
	})

	t.Run("the earlier run models keep their frozen contexts", func(t *testing.T) {
		executiontest.SourceOldRunModelsUnchanged(t)
	})
}

var (
	sequenced = regexp.MustCompile(`run[-_]repo[-_]\d+`)
	digested  = regexp.MustCompile(`"sourceSetDigest":"sha256:[0-9a-f]{64}"`)
)
