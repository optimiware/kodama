//go:build integration

package identity_test

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/shell/app/drivers/userlandfs"
	"github.com/optimiware/kodama/internal/userland/cli"
	gitapp "github.com/optimiware/kodama/internal/userland/git/app"
	idfix "github.com/optimiware/kodama/test/support/boundaries/identity"
)

// diagnosticCanary is planted in a failing flow as the unknown service
// credential. It must never be rendered by a diagnostic.
const diagnosticCanary = "kodsvc.canary.LEAKCANARY0123456789"

// actorCanary is planted in a failing flow as the unknown actor credential. It
// must never be rendered by a diagnostic.
const actorCanary = "kodex.execution-canary.LEAKCANARY0123456789"

func backgroundContext() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}

func runCLICall(t *testing.T, ctx context.Context, transport client.Transport, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := cli.Run(ctx, cli.Config{ServerURL: "https://factory.example", Output: cli.OutputJSON}, args, cli.Ports{
		Client: transport,
		Out:    &out,
		Err:    &errOut,
		Now:    func() time.Time { return idfix.DefaultClockTime() },
	})
	return code, out.String(), errOut.String()
}

func verifyFailureTexts(t *testing.T, provider *idfix.Provider, source client.CredentialSource, actor string, failure *idfix.Failure, viaCLI bool) []string {
	t.Helper()
	server := newContract(t, provider, defaultContractOptions())
	if failure != nil {
		server.QueueVerifyFailure(*failure)
	}
	transport := newTransport(t, server, source, 1)
	if viaCLI {
		code, out, errOut := runCLICall(t, context.Background(), transport, "call", "actor.verify", "--input="+string(mustJSON(t, validVerifyRequest(actor))))
		if code != cli.ExitFailure {
			t.Fatalf("CLI verify exit code = %d; want failure", code)
		}
		if out != "" {
			t.Fatalf("CLI verify wrote stdout on failure: %q", out)
		}
		return []string{errOut}
	}
	verification, err := gitapp.NewVerificationClient(transport)
	if err != nil {
		t.Fatalf("NewVerificationClient: %v", err)
	}
	if _, verifyErr := verification.Verify(context.Background(), validVerifyRequest(actor)); verifyErr == nil {
		t.Fatal("Verify succeeded against a failing endpoint")
	} else {
		return []string{verifyErr.Error()}
	}
	return nil
}

func reportFailureTexts(t *testing.T, provider *idfix.Provider, humanToken string, source client.CredentialSource, failure *idfix.Failure, ctx context.Context, viaCLI bool) []string {
	t.Helper()
	server := newContract(t, provider, defaultContractOptions())
	verified := mustVerify(t, server, humanToken)
	request := validReportRequest(verified.VerificationID)
	if failure != nil {
		server.QueueReportFailure(*failure)
	}
	transport := newTransport(t, server, source, 1)
	if viaCLI {
		code, out, errOut := runCLICall(t, ctx, transport,
			"call", "resource.report",
			"--input="+string(mustJSON(t, request)),
			"--idempotency-key="+request.SourceEventID,
		)
		if code != cli.ExitFailure {
			t.Fatalf("CLI report exit code = %d; want failure", code)
		}
		if out != "" {
			t.Fatalf("CLI report wrote stdout on failure: %q", out)
		}
		return []string{errOut}
	}
	verification, err := gitapp.NewVerificationClient(transport)
	if err != nil {
		t.Fatalf("NewVerificationClient: %v", err)
	}
	if _, reportErr := verification.Report(ctx, request); reportErr == nil {
		t.Fatal("Report succeeded against a failing endpoint")
	} else {
		return []string{reportErr.Error()}
	}
	return nil
}

func diagnosticSecretRules(humanToken string) []secretRule {
	return []secretRule{
		{label: "service token", value: fixtureServiceTok},
		{label: "human token", value: humanToken},
		{label: "execution token", value: fixtureExecutionTok},
		{label: "client secret", value: "client-secret"},
		{label: "service canary", value: diagnosticCanary},
		{label: "actor canary", value: actorCanary},
	}
}

func TestFailedVerifyDiagnosticsNeverRenderCredentials(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	human, err := provider.MintAccessToken("auth0|human-1", fixtureTenant, fixtureProject)
	if err != nil {
		t.Fatalf("MintAccessToken: %v", err)
	}
	rules := diagnosticSecretRules(human.Value)

	cases := []struct {
		name    string
		source  client.CredentialSource
		actor   string
		failure *idfix.Failure
	}{
		{"provider 5xx", serviceSource(), human.Value, &idfix.Failure{Mode: idfix.FailureStatus, Status: http.StatusServiceUnavailable}},
		{"dropped response", serviceSource(), human.Value, &idfix.Failure{Mode: idfix.FailureDropAfterCommit}},
		{"corrupt success envelope", serviceSource(), human.Value, &idfix.Failure{Mode: idfix.FailureExtraField}},
		{"unknown service caller", tokenSource(client.KindService, diagnosticCanary), human.Value, nil},
		{"unknown actor credential", serviceSource(), actorCanary, nil},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			requireNoLeakedSecrets(t, testCase.name+" returned error",
				verifyFailureTexts(t, provider, testCase.source, testCase.actor, testCase.failure, false), rules...)
			requireNoLeakedSecrets(t, testCase.name+" CLI stderr",
				verifyFailureTexts(t, provider, testCase.source, testCase.actor, testCase.failure, true), rules...)
		})
	}
}

func TestFailedReportDiagnosticsNeverRenderCredentials(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	human, err := provider.MintAccessToken("auth0|human-1", fixtureTenant, fixtureProject)
	if err != nil {
		t.Fatalf("MintAccessToken: %v", err)
	}
	rules := diagnosticSecretRules(human.Value)

	cases := []struct {
		name    string
		source  client.CredentialSource
		failure *idfix.Failure
		ctx     func() (context.Context, context.CancelFunc)
	}{
		{"provider 5xx", serviceSource(), &idfix.Failure{Mode: idfix.FailureStatus, Code: "internal_failure", Status: http.StatusInternalServerError}, backgroundContext},
		{"dropped response", serviceSource(), &idfix.Failure{Mode: idfix.FailureDropAfterCommit}, backgroundContext},
		{"corrupt success envelope", serviceSource(), &idfix.Failure{Mode: idfix.FailureExtraField}, backgroundContext},
		{"timeout", serviceSource(), &idfix.Failure{Mode: idfix.FailureDelay, Delay: 200 * time.Millisecond}, func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 30*time.Millisecond)
		}},
		{"unknown service caller", tokenSource(client.KindService, diagnosticCanary), nil, backgroundContext},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, cancel := testCase.ctx()
			defer cancel()
			requireNoLeakedSecrets(t, testCase.name+" returned error",
				reportFailureTexts(t, provider, human.Value, testCase.source, testCase.failure, ctx, false), rules...)
			ctx, cancel = testCase.ctx()
			defer cancel()
			requireNoLeakedSecrets(t, testCase.name+" CLI stderr",
				reportFailureTexts(t, provider, human.Value, testCase.source, testCase.failure, ctx, true), rules...)
		})
	}
}

// TestDiagnosticScannerDetectsPlantedCanary proves the diagnostic scanner can
// fail: a deliberately leaky stand-in transport renders its credential and the
// same scanner reports it.
func TestDiagnosticScannerDetectsPlantedCanary(t *testing.T) {
	canary := "LEAKCANARY-diagnostic-0123456789"
	code, out, errOut := runCLICall(t, context.Background(), leakyTransport{secret: canary},
		"call", "execution.inspect", "--input={\"execution_id\":\"execution-1\"}")
	if code != cli.ExitFailure {
		t.Fatalf("leaky transport exit code = %d; want failure", code)
	}
	violations := findLeakedSecrets([]string{out, errOut}, []secretRule{{label: "canary", value: canary}})
	if len(violations) == 0 {
		t.Fatal("the diagnostic scanner did not detect a planted canary in the leaky stand-in")
	}
}

// TestPersistenceScannerDetectsLeakedSessionCopy proves the persistence
// scanner can fail: a deliberately leaky filesystem stand-in writes an extra
// sidecar copy of the session and the same scanner reports it.
func TestPersistenceScannerDetectsLeakedSessionCopy(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	fixture := sessionFixture(t, provider)
	store := &cli.FileSessionStore{
		Path:  fixture.path,
		Files: leakyFiles{FileSystem: userlandfs.FileSystem{}},
		Now:   provider.Clock().Now,
	}
	code, _, errOut := runLogin(t, provider, store, fixture.credential)
	if code != cli.ExitSuccess {
		t.Fatalf("login with the leaky stand-in = %d/%q", code, errOut)
	}
	violations, err := findPersistedSecrets(fixture.root, []secretRule{{
		label: "user token", value: fixture.credential.Token, allowed: fixture.path,
	}})
	if err != nil {
		t.Fatalf("scan persisted secrets: %v", err)
	}
	if len(violations) == 0 {
		t.Fatal("the persistence scanner did not detect the leaked session sidecar")
	}
}

// TestFixtureRecordScannersDetectPlantedCanary proves the fixture-record
// redaction scans can fail: a canary placed in a recorded identifier is
// detected by the same scanner used on the fixture records and logs.
func TestFixtureRecordScannersDetectPlantedCanary(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	options := defaultContractOptions()
	options.Services[0].ID = diagnosticCanary
	options.Services[0].Token = "canary-token"
	server := newContract(t, provider, options)
	body := mustJSON(t, map[string]any{"input": validVerifyRequest(actorCanary)})
	response := postRawWithAuth(t, server, "/api/v1/verify", "canary-token", "application/json", body)
	_ = readBody(t, response)
	rules := []secretRule{{label: "canary", value: diagnosticCanary}}
	texts := []string{server.RecordedJSON(), server.LogText()}
	if len(findLeakedSecrets(texts, rules)) == 0 {
		t.Fatal("the fixture-record scanner did not detect a planted canary")
	}

	// The OIDC provider's safe request log is scanned by the token-endpoint
	// test. A canary client id proves that scan can fail when a credential
	// value reaches the log.
	oidcProvider := newOIDC(t, idfix.OIDCOptions{})
	if err := oidcProvider.RegisterClient(diagnosticCanary, "canary-secret", nil); err != nil {
		t.Fatalf("RegisterClient: %v", err)
	}
	code, err := oidcProvider.IssueAuthorizationCode(diagnosticCanary, "", nil)
	if err != nil {
		t.Fatalf("IssueAuthorizationCode: %v", err)
	}
	if _, err := oidcProvider.ExchangeCode(context.Background(), code, diagnosticCanary, "canary-secret", ""); err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if len(findLeakedSecrets([]string{oidcProvider.LogText()}, rules)) == 0 {
		t.Fatal("the OIDC log scanner did not detect a planted canary")
	}
}
