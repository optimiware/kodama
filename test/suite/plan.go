// Package suite plans and runs Kodama's real-infrastructure test suites as
// waves of concurrent lanes, for a Podman installation, a local kind cluster,
// the cloud stage and the complete local matrix.
//
// The same portable product proofs (test/e2e, driven only by an installation
// descriptor) run unchanged on every runtime. Work is ordered by blast radius,
// not by qualification card: everything that only touches its own objects runs
// concurrently in the first wave, and the probes that disrupt shared services
// run alone in a second wave that starts only after the first one passed.
//
// A plan carries no credential: lanes receive private token-file paths, never
// token values.
package suite

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// Target names the installation topology a plan drives.
type Target string

const (
	// TargetPodman runs the portable e2e proofs against a Podman installation
	// together with the local restore drills.
	TargetPodman Target = "podman"
	// TargetKind runs the kind-backed suites, or the portable e2e proofs when
	// a descriptor names a Kubernetes installation.
	TargetKind Target = "kind"
	// TargetStage runs the stage qualification: the portable e2e proofs, the
	// non-disruptive production probes and the RDB restore drill together,
	// then the chaos probes alone.
	TargetStage Target = "stage"
	// TargetLocalAll runs every local stack and kind suite concurrently.
	TargetLocalAll Target = "local-all"
)

// slotLauncher bounds how many real-infrastructure runs a host starts at once.
const slotLauncher = "scripts/infra-slot.sh"

// testVerifier fails a go test lane when a selected test did not run or skipped.
const testVerifier = "test/e2e/internal/harness/verify-go-test.sh"

// The concurrency knobs of the gated test packages and their accepted maxima.
// A lane never passes a value its package would refuse.
const (
	e2eConcurrencyEnv   = "KODAMA_E2E_CONCURRENCY"
	stageConcurrencyEnv = "KODAMA_STAGE_CONCURRENCY"
	q03ConcurrencyEnv   = "KODAMA_Q03_CONCURRENCY"
	kindConcurrencyEnv  = "KODAMA_LIFECYCLE_KIND_CONCURRENCY"

	maxE2EConcurrency   = 32
	maxStageConcurrency = 24
	maxQ03Concurrency   = 8
	maxKindConcurrency  = 6
)

// MaxConcurrency is the largest accepted Options.Concurrency.
const MaxConcurrency = maxE2EConcurrency

// podmanPortEnv is the complete port set deploy/podman/smoke.sh requires for
// its disposable installation.
var podmanPortEnv = []string{
	"KODAMA_LOCAL_HTTPS_PORT",
	"KODAMA_LOCAL_OIDC_PORT",
	"KODAMA_LOCAL_GIT_PORT",
	"KODAMA_LOCAL_GIT_HTTPS_PORT",
	"KODAMA_LOCAL_POSTGRES_PORT",
	"KODAMA_LOCAL_NATS_PORT",
	"KODAMA_LOCAL_NATS_MONITOR_PORT",
	"KODAMA_LOCAL_OAUTH_UPSTREAM_PORT",
}

// Options selects and sizes one plan.
type Options struct {
	Target Target
	// Installation is the schema version 1 descriptor of an existing
	// installation. The stage target requires it; the podman and kind targets
	// run the portable e2e proofs against it when it is set.
	Installation string
	// EvidenceDir is the absolute directory that receives one evidence
	// directory per lane.
	EvidenceDir string
	// Concurrency overrides every lane's scenario concurrency; 0 keeps each
	// lane's default. A lane caps the value at what its package accepts.
	Concurrency int
	// Race runs every go test lane with the race detector.
	Race bool
	// SkipChaos leaves out the stage's second, disruptive wave.
	SkipChaos bool
	// SkipQ03 leaves out the restore drills.
	SkipQ03 bool
	// Q03Mode is the stage RDB drill mode: plan (default), restore or drop.
	Q03Mode string
	// SecondAdminTokenFile is the private token file of the second tenant's
	// administrator, when the stage run acquired one.
	SecondAdminTokenFile string
}

// Lane is one command. The lanes of a wave run concurrently.
type Lane struct {
	Name string   `json:"name"`
	Argv []string `json:"argv"`
	// Env holds the KEY=VALUE settings added to the inherited environment.
	Env []string `json:"env,omitempty"`
}

// Wave is a group of lanes that starts only after the previous wave finished.
type Wave struct {
	Name  string `json:"name"`
	Lanes []Lane `json:"lanes"`
}

// Plan is the ordered wave sequence of one target.
type Plan struct {
	Target Target `json:"target"`
	Waves  []Wave `json:"waves"`
}

// BuildPlan returns the plan for options. getenv supplies the caller's
// environment: an input the caller exported is never overridden by a derived
// value.
func BuildPlan(options Options, getenv func(string) string) (Plan, error) {
	if options.Concurrency < 0 || options.Concurrency > MaxConcurrency {
		return Plan{}, fmt.Errorf("concurrency %d is outside [0, %d]", options.Concurrency, MaxConcurrency)
	}
	if options.EvidenceDir == "" || !filepath.IsAbs(options.EvidenceDir) {
		return Plan{}, fmt.Errorf("the evidence directory %q must be an absolute path", options.EvidenceDir)
	}
	var (
		waves []Wave
		err   error
	)
	switch options.Target {
	case TargetPodman:
		waves, err = podmanWaves(options, getenv)
	case TargetKind:
		waves = kindWaves(options)
	case TargetStage:
		waves, err = stageWaves(options, getenv)
	case TargetLocalAll:
		waves = localAllWaves(options, getenv)
	default:
		err = fmt.Errorf("unknown target %q (want podman, kind, stage or local-all)", options.Target)
	}
	if err != nil {
		return Plan{}, err
	}
	return Plan{Target: options.Target, Waves: waves}, nil
}

// concurrency returns the lane's scenario concurrency: the caller's override
// capped at the package maximum, or the lane default.
func (options Options) concurrency(fallback, maximum int) int {
	if options.Concurrency == 0 {
		return fallback
	}
	return min(options.Concurrency, maximum)
}

// evidence returns the lane's evidence directory.
func (options Options) evidence(lane string) string {
	return filepath.Join(options.EvidenceDir, lane)
}

// goTest returns one go test command line.
func (options Options) goTest(timeout, tags string, arguments ...string) []string {
	argv := []string{"go", "test"}
	if options.Race {
		argv = append(argv, "-race")
	}
	argv = append(argv, "-count=1", "-timeout", timeout, "-tags="+tags)
	return append(argv, arguments...)
}

// verifiedGoTest wraps goTest in verify-go-test.sh with an explicit expected
// test list and -json output so a zero-test selection or skipped proof fails.
func (options Options) verifiedGoTest(expected []string, timeout, tags string, arguments ...string) []string {
	argv := make([]string, 0, 2+2*len(expected)+8+len(arguments))
	argv = append(argv, testVerifier)
	for _, name := range expected {
		argv = append(argv, "--expect", name)
	}
	argv = append(argv, "--")
	withJSON := make([]string, 0, 1+len(arguments))
	withJSON = append(withJSON, "-json")
	withJSON = append(withJSON, arguments...)
	return append(argv, options.goTest(timeout, tags, withJSON...)...)
}

// inSlot runs a command inside one slot of a launcher pool.
func inSlot(pool string, command ...string) []string {
	return append([]string{slotLauncher, pool, "--"}, command...)
}

// setting renders one KEY=VALUE environment entry.
func setting(name string, value any) string {
	return fmt.Sprintf("%s=%v", name, value)
}

// portableE2ELane runs the portable proofs against the described installation.
// -parallel follows the scenario concurrency, so the package gate, not the
// host's CPU count, is the bound.
func (options Options) portableE2ELane(name, pool string, concurrency int) Lane {
	return Lane{
		Name: name,
		Argv: inSlot(pool, options.verifiedGoTest(PortableE2ETests, "30m", "e2e",
			"-parallel", strconv.Itoa(concurrency), "-run", PortableE2ERunPattern, "./test/e2e/")...),
		Env: []string{
			"KODAMA_REQUIRE_INFRA=1",
			"Q01_REQUIRE_INSTALLATION=1",
			"Q01_SMOKE_GIT=1",
			setting("Q01_INSTALLATION_FILE", options.Installation),
			setting("Q01_EVIDENCE_DIR", options.evidence(name)),
			setting("KODAMA_EVIDENCE_DIR", options.evidence(name)),
			setting(e2eConcurrencyEnv, concurrency),
		},
	}
}

// localRestoreLane runs the local restore drills.
func (options Options) localRestoreLane(getenv func(string) string) Lane {
	const name = "qualification-restore-local"
	env := []string{
		"KODAMA_REQUIRE_INFRA=1",
		setting("KODAMA_EVIDENCE_DIR", options.evidence(name)),
		setting(q03ConcurrencyEnv, options.concurrency(4, maxQ03Concurrency)),
	}
	if strings.TrimSpace(getenv("KODAMA_Q03_SOURCE_REVISION")) == "" {
		for _, candidate := range []string{"Q01_DEPLOYED_REVISION", "Q01_HARNESS_REVISION"} {
			if rev := strings.TrimSpace(getenv(candidate)); rev != "" {
				env = append(env, setting("KODAMA_Q03_SOURCE_REVISION", rev))
				break
			}
		}
	}
	return Lane{
		Name: name,
		Argv: inSlot("stack", options.goTest("25m", "qualification", "./test/qualification/restore/")...),
		Env:  env,
	}
}

// kindLifecycleLane runs the lifecycle tests that own a kind cluster.
func (options Options) kindLifecycleLane() Lane {
	const name = "integration-lifecycle-kind"
	return Lane{
		Name: name,
		Argv: inSlot("kind", options.verifiedGoTest(KindLifecycleTests, "45m", "integration", "-run", "^TestKubernetes", "./test/integration/lifecycle/")...),
		Env: []string{
			"KODAMA_REQUIRE_INFRA=1",
			setting("KODAMA_EVIDENCE_DIR", options.evidence(name)),
			setting(kindConcurrencyEnv, options.concurrency(3, maxKindConcurrency)),
		},
	}
}

// kindComponentLane runs the Kubernetes adapter and deployment components.
func (options Options) kindComponentLane() Lane {
	const name = "component-kubernetes-deploy"
	return Lane{
		Name: name,
		Argv: inSlot("kind", options.goTest("40m", "component", "./test/component/kubernetes/", "./test/component/deploy/")...),
		Env: []string{
			"KODAMA_REQUIRE_INFRA=1",
			setting("KODAMA_EVIDENCE_DIR", options.evidence(name)),
		},
	}
}

func podmanWaves(options Options, getenv func(string) string) ([]Wave, error) {
	concurrency := options.concurrency(8, maxE2EConcurrency)
	var lanes []Lane
	if options.Installation != "" {
		lanes = append(lanes, options.portableE2ELane("portable-e2e-podman", "stack", concurrency))
	} else {
		lane, err := options.disposablePodmanLane(concurrency, getenv)
		if err != nil {
			return nil, err
		}
		lanes = append(lanes, lane)
	}
	if !options.SkipQ03 {
		lanes = append(lanes, options.localRestoreLane(getenv))
	}
	return []Wave{{Name: "wave-1-podman", Lanes: lanes}}, nil
}

// disposablePodmanLane installs a throwaway Podman installation with
// deploy/podman/smoke.sh, runs the portable proofs against it through the
// verifying wrapper and purges it. The installation gets extra repositories so
// the protected-Git proofs do not take turns on one.
func (options Options) disposablePodmanLane(concurrency int, getenv func(string) string) (Lane, error) {
	var missing []string
	for _, name := range podmanPortEnv {
		if strings.TrimSpace(getenv(name)) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return Lane{}, fmt.Errorf("the podman target needs an existing installation (--installation) or the port set of a disposable one; unset: %s",
			strings.Join(missing, ", "))
	}
	const name = "portable-e2e-podman"
	environment := []string{
		"Q01_SMOKE_GIT=1",
		setting("KODAMA_SMOKE_TEST_RUN", PortableE2ERunPattern),
		setting("KODAMA_SMOKE_EXPECT", strings.Join(PortableE2ETests, " ")),
		"KODAMA_SMOKE_SHARD_REPOS=2",
		setting("Q01_EVIDENCE_DIR", options.evidence(name)),
		setting("KODAMA_EVIDENCE_DIR", options.evidence(name)),
		setting(e2eConcurrencyEnv, concurrency),
	}
	if strings.TrimSpace(getenv("Q01_NATS_URL")) == "" {
		// The live subscription proofs need the installation's broker.
		environment = append(environment, setting("Q01_NATS_URL", "nats://127.0.0.1:"+strings.TrimSpace(getenv("KODAMA_LOCAL_NATS_PORT"))))
	}
	return Lane{Name: name, Argv: inSlot("stack", "deploy/podman/smoke.sh"), Env: environment}, nil
}

func kindWaves(options Options) []Wave {
	var lanes []Lane
	if options.Installation != "" {
		lanes = []Lane{options.portableE2ELane("portable-e2e-kind", "kind", options.concurrency(6, maxE2EConcurrency))}
	} else {
		lanes = []Lane{options.kindLifecycleLane(), options.kindComponentLane()}
	}
	return []Wave{{Name: "wave-1-kind", Lanes: lanes}}
}

// localAllWaves mirrors the Makefile's suites-stack and suites-kind rows as
// concurrent lanes. The lifecycle package is split by runtime: its Podman tests
// take a stack slot and its kind tests a kind slot.
func localAllWaves(options Options, getenv func(string) string) []Wave {
	stack := func(name, timeout, tags string, arguments ...string) Lane {
		return Lane{
			Name: name,
			Argv: inSlot("stack", options.goTest(timeout, tags, arguments...)...),
			Env: []string{
				"KODAMA_REQUIRE_INFRA=1",
				setting("KODAMA_EVIDENCE_DIR", options.evidence(name)),
			},
		}
	}
	lanes := []Lane{
		stack("integration-factory", "60m", "integration", "./test/integration/factory/"),
		stack("integration-services", "40m", "integration", "./test/integration/services/"),
		stack("integration-composition-boundaries", "30m", "integration", "./test/integration/composition/...", "./test/integration/boundaries/..."),
		stack("integration-lifecycle-podman", "35m", "integration", "-skip", "^TestKubernetes", "./test/integration/lifecycle/"),
		stack("component-postgres", "30m", "component", "./test/component/postgres/"),
		stack("component-stack", "30m", "component", "./test/component/messaging/", "./test/component/runtime/", "./test/component/recovery/", "./test/integration/composition/..."),
		options.kindLifecycleLane(),
		options.kindComponentLane(),
	}
	if !options.SkipQ03 {
		lanes = append(lanes, options.localRestoreLane(getenv))
	}
	return []Wave{{Name: "wave-1-local", Lanes: lanes}}
}

func stageWaves(options Options, getenv func(string) string) ([]Wave, error) {
	if options.Installation == "" {
		return nil, errors.New("the stage target needs an installation descriptor (--installation or Q01_INSTALLATION_FILE)")
	}
	mode := options.Q03Mode
	if mode == "" {
		mode = "plan"
	}
	switch mode {
	case "plan", "restore", "drop":
	default:
		return nil, fmt.Errorf("unknown Q03 mode %q (want plan, restore or drop)", mode)
	}
	if !options.SkipQ03 && strings.TrimSpace(getenv("KODAMA_Q03_STAGE_INSTANCE_ID")) == "" {
		return nil, errors.New("the stage RDB drill is not configured: set the KODAMA_Q03_STAGE_* inputs of scripts/q03-stage-drill or pass --skip-q03")
	}
	environment, err := stageEnvironment(options, getenv)
	if err != nil {
		return nil, err
	}

	// The stage lanes run inside the stage lock their caller already holds, so
	// they take no slot of their own.
	e2eConcurrency := options.concurrency(6, maxE2EConcurrency)
	probeConcurrency := options.concurrency(6, maxStageConcurrency)
	first := []Lane{
		{
			Name: "stage-e2e",
			Argv: options.verifiedGoTest(PortableE2ETests, "30m", "e2e",
				"-parallel", strconv.Itoa(e2eConcurrency), "-run", PortableE2ERunPattern, "./test/e2e/"),
			Env: append(environment.e2e, setting(e2eConcurrencyEnv, e2eConcurrency)),
		},
		{
			Name: "stage-probes",
			Argv: options.verifiedGoTest(StageWave1Probes, "30m", "qualification",
				"-parallel", strconv.Itoa(probeConcurrency), "-run", StageWave1RunPattern(), "./test/qualification/production/"),
			Env: append(environment.probes("stage-probes"), setting(stageConcurrencyEnv, probeConcurrency)),
		},
	}
	if !options.SkipQ03 {
		const name = "stage-rdb-drill"
		lane := Lane{Name: name, Argv: []string{"scripts/q03-stage-drill", mode}}
		if strings.TrimSpace(getenv("KODAMA_Q03_EVIDENCE_DIR")) == "" {
			lane.Env = append(lane.Env, setting("KODAMA_Q03_EVIDENCE_DIR", options.evidence(name)))
		}
		if strings.TrimSpace(getenv("KODAMA_Q03_SOURCE_REVISION")) == "" && environment.revision != "" {
			lane.Env = append(lane.Env, setting("KODAMA_Q03_SOURCE_REVISION", environment.revision))
		}
		first = append(first, lane)
	}
	waves := []Wave{{Name: "wave-1-concurrent", Lanes: first}}
	if !options.SkipChaos {
		waves = append(waves, Wave{Name: "wave-2-chaos", Lanes: []Lane{{
			Name: "stage-chaos",
			Argv: options.verifiedGoTest(StageWave2ChaosProbes, "60m", "qualification",
				"-parallel", "1", "-run", StageWave2ChaosRunPattern(), "./test/qualification/production/"),
			Env: environment.probes("stage-chaos"),
		}}})
	}
	return waves, nil
}
