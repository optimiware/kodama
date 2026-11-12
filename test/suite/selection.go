package suite

import "strings"

// PortableE2ERunPattern selects the portable product proofs of test/e2e: every
// acceptance-criterion test, the surface parity probe and the installation
// preflight. It leaves out the package's hermetic helper tests and
// TestReleaseSmoke, whose five scenarios the AC04, AC05, AC14, AC17 and AC18
// tests run with their acceptance evidence.
const PortableE2ERunPattern = "^TestE2E_(AC|Parity$|InstallationAccess$)"

// PortableE2ETests names every test PortableE2ERunPattern selects. A
// disposable-installation run passes the list to the verifying wrapper, so a
// removed, renamed or skipped proof fails the run instead of shrinking it.
var PortableE2ETests = []string{
	"TestE2E_AC01InstalledPipeline",
	"TestE2E_AC02ImmutableAdmission",
	"TestE2E_AC03Idempotency",
	"TestE2E_AC04RequestSettlement",
	"TestE2E_AC05ProtectedIntegration",
	"TestE2E_AC06Recovery",
	"TestE2E_AC07Identity",
	"TestE2E_AC08DeliveryEvidence",
	"TestE2E_AC09CleanupAttributable",
	"TestE2E_AC10OptionalCapabilities",
	"TestE2E_AC11Materialization",
	"TestE2E_AC12Outputs",
	"TestE2E_AC13OrderedActions",
	"TestE2E_AC14Integrations",
	"TestE2E_AC14LiveEventDelivery",
	"TestE2E_AC14LiveLogDelivery",
	"TestE2E_AC15BrokerIndependence",
	"TestE2E_AC16PlannedWait",
	"TestE2E_AC17ConversationHandoffs",
	"TestE2E_AC18TaskProgress",
	"TestE2E_AC19AuthorizationVerification",
	"TestE2E_AC20UserlandGit",
	"TestE2E_InstallationAccess",
	"TestE2E_Parity",
}

// SerialE2ETests names the portable proofs that assert on project-wide state
// and therefore run alone, before the parallel proofs start. Every other
// portable proof calls e2eParallel first.
var SerialE2ETests = []string{
	// Compares the page after the project's event tail across three edges.
	"TestE2E_Parity",
}

// StageWave1Probes are the live test/qualification/production probes that
// touch only their own tenant workloads and API objects. They run in parallel
// with each other, with the portable e2e proofs and with the RDB restore drill.
var StageWave1Probes = []string{
	"TestStageAuthenticatedAndUnauthenticated",
	"TestStageAuthorityRESTReads",
	"TestStageAuthorityMCPReads",
	"TestStageAuthorityRoleRefusal",
	"TestStageAuthorityServiceLifecycle",
	"TestStageAuthorityCredentialRenewal",
	"TestStageRequestWaitRevocation",
	"TestStageRuntimeIsolation",
	"TestStageCrossTenantIsolation",
	"TestStageResilienceCleanupAttribution",
	"TestStageResilienceUncertainStopBlocksHandoff",
	"TestStageResilienceWorkspaceLoss",
	"TestStageResilienceWorkloadPodLoss",
}

// StageWave2ChaosProbes delete or scale the shared kodama-system workloads
// (controller, broker, Git). They run one at a time, after wave 1 has drained
// and passed, so no other scenario observes the outage they cause.
var StageWave2ChaosProbes = []string{
	"TestStageResilienceControllerPodDeletion",
	"TestStageResilienceCleanupCrashAttribution",
	"TestStageResilienceNATSPodDeletion",
	"TestStageResilienceBrokerOutage",
	"TestStageResilienceGitDependencyOutage",
}

// StageUnscheduledProbes are TestStage* placeholders that only skip. No wave
// selects them, so a stage run reports no skip for a probe that was never
// written.
var StageUnscheduledProbes []string

// KindLifecycleTests names every test ^TestKubernetes selects in
// test/integration/lifecycle. The kind lifecycle lane passes the list to the
// verifying wrapper so a removed, renamed or skipped test fails the run.
var KindLifecycleTests = []string{
	"TestKubernetesCancellationStopsAdoptedWorkload",
	"TestKubernetesControllerRestart",
	"TestKubernetesLostStartResponseAcrossRestart",
	"TestKubernetesLostStopResponseAcrossRestart",
	"TestKubernetesLostWorkspaceFailsWithoutReplay",
	"TestKubernetesPendingCancellationCompletesCleanup",
	"TestKubernetesPipeline",
	"TestKubernetesPreparationFailureCleanup",
	"TestKubernetesUncertainStopBlocksHandoff",
}

// StageWave1RunPattern is the anchored -run expression of wave 1.
func StageWave1RunPattern() string { return exactRunPattern(StageWave1Probes) }

// StageWave2ChaosRunPattern is the anchored -run expression of wave 2.
func StageWave2ChaosRunPattern() string { return exactRunPattern(StageWave2ChaosProbes) }

func exactRunPattern(names []string) string {
	return "^(" + strings.Join(names, "|") + ")$"
}
