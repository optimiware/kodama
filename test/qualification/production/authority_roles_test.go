//go:build qualification

package production

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
)

// roleAuthorityProbe is the whole role-refusal probe. It creates a lower
// privilege actor that exists on the stage through the public API (a viewer
// service account), proves it can read, proves it cannot perform two operations
// its role never grants, and disables it at the end. It never uses the
// developer persona: the stage has no principal for it.
type roleAuthorityProbe struct {
	client      *API
	adminToken  Secret
	projectID   string
	principalID string

	serviceID   string
	version     uint64
	disabled    bool
	viewerToken Secret
	issued      []Secret
}

// disable disables the viewer service at its last known version. It is
// idempotent and never logs a credential.
func (probe *roleAuthorityProbe) disable(ctx context.Context) error {
	if probe.serviceID == "" || probe.disabled {
		return nil
	}
	key, err := probeIdempotencyKey("q02-viewer-disable")
	if err != nil {
		return err
	}
	result, err := probe.client.PostVersioned(ctx, probe.adminToken, "service.disable",
		api.ServiceDisableRequest{ServiceID: probe.serviceID}, key, probe.version)
	if err != nil {
		return fmt.Errorf("disable viewer service %q: %w", probe.serviceID, err)
	}
	disabled, err := decodeStageResult[api.ServiceDisableResult](result, "service.disable")
	if err != nil {
		return fmt.Errorf("disable viewer service %q: %w", probe.serviceID, err)
	}
	probe.version = disabled.Version
	probe.disabled = true
	return nil
}

// readExecutionList performs the viewer's permitted read.
func (probe *roleAuthorityProbe) readExecutionList(ctx context.Context, token Secret) error {
	result, err := probe.client.Get(ctx, token, "execution.list",
		api.ExecutionListRequest{ProjectID: probe.projectID}, "")
	if err != nil {
		return err
	}
	_, err = decodeStageResult[api.ExecutionListResult](result, "execution.list")
	return err
}

// run creates the viewer service, proves its positive read, proves the two
// refused operations return 403 forbidden (authenticated, not authorized), and
// keeps the admin's positive execution.list control. An accidental allow of
// membership.grant is reverted with the admin token at the returned version; if
// the admin cannot revoke, the failure names the bounded leftover principal.
func (probe *roleAuthorityProbe) run(ctx context.Context) (err error) {
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		disableErr := probe.disable(cleanupCtx)
		switch {
		case disableErr != nil && err == nil:
			err = disableErr
		case disableErr != nil:
			err = fmt.Errorf("%w (the viewer service %q could not be disabled: %v)", err, probe.serviceID, disableErr)
		}
	}()

	serviceID, err := probeID("q02-authority-viewer")
	if err != nil {
		return fmt.Errorf("generate the viewer service identity: %w", err)
	}
	probe.serviceID = serviceID
	createKey, err := probeIdempotencyKey("q02-viewer-create")
	if err != nil {
		return err
	}
	createResult, err := probe.client.Post(ctx, probe.adminToken, "service.create",
		api.ServiceCreateRequest{ProjectID: probe.projectID, ServiceID: serviceID, Roles: []string{"viewer"}}, createKey)
	if err != nil {
		return fmt.Errorf("create the viewer service %q: %w", serviceID, err)
	}
	created, err := decodeStageResult[api.ServiceCreateResult](createResult, "service.create")
	if err != nil {
		return fmt.Errorf("create the viewer service %q: %w", serviceID, err)
	}
	probe.version = created.Version

	issueKey, err := probeIdempotencyKey("q02-viewer-issue")
	if err != nil {
		return err
	}
	issueResult, err := probe.client.PostVersioned(ctx, probe.adminToken, "service.credential.issue",
		api.ServiceCredentialIssueRequest{ServiceID: serviceID, TTLSeconds: 900}, issueKey, probe.version)
	if err != nil {
		return fmt.Errorf("issue the viewer service %q credential: %w", serviceID, err)
	}
	issued, err := decodeStageResult[api.ServiceCredentialIssueResult](issueResult, "service.credential.issue")
	if err != nil {
		return fmt.Errorf("issue the viewer service %q credential: %w", serviceID, err)
	}
	viewerToken, err := stageCredential(issued.Token, serviceCredentialPrefix)
	if err != nil {
		return fmt.Errorf("issue the viewer service %q credential: %w", serviceID, err)
	}
	probe.viewerToken = viewerToken
	probe.version = issued.Version
	probe.issued = append(probe.issued, viewerToken)

	if err := probe.readExecutionList(ctx, viewerToken); err != nil {
		return fmt.Errorf("the viewer service's positive execution.list was refused: %w", err)
	}

	grantKey, err := probeIdempotencyKey("q02-viewer-grant")
	if err != nil {
		return err
	}
	grant := api.MembershipGrantRequest{
		ProjectID:   probe.projectID,
		PrincipalID: probe.principalID,
		Roles:       []string{"viewer"},
	}
	// membership.grant is the frozen PUT binding.
	grantResult, grantErr := probe.client.Put(ctx, viewerToken, "membership.grant", grant, grantKey)
	if grantErr == nil {
		granted, decodeErr := decodeStageResult[api.MembershipGrantResult](grantResult, "membership.grant")
		if decodeErr != nil {
			return fmt.Errorf("the viewer was allowed the admin-only membership.grant and returned an invalid result: %w (throwaway principal %q may remain)", decodeErr, probe.principalID)
		}
		revertKey, keyErr := probeIdempotencyKey("q02-viewer-revert")
		if keyErr != nil {
			return fmt.Errorf("the viewer was allowed the admin-only membership.grant; cannot build a revert key (throwaway principal %q may remain)", probe.principalID)
		}
		revertResult, revertErr := probe.client.DeleteVersioned(ctx, probe.adminToken, "membership.revoke",
			api.MembershipRevokeRequest{ProjectID: probe.projectID, PrincipalID: probe.principalID},
			revertKey, granted.Version)
		if revertErr != nil {
			return fmt.Errorf("the viewer was allowed the admin-only membership.grant and the admin could not revoke throwaway principal %q: %v (leftover membership is bounded to that principal)", probe.principalID, revertErr)
		}
		if _, err := decodeStageResult[api.MembershipRevokeResult](revertResult, "membership.revoke"); err != nil {
			return fmt.Errorf("the viewer was allowed the admin-only membership.grant; the revert of throwaway principal %q returned an invalid result: %w", probe.principalID, err)
		}
		return fmt.Errorf("the viewer was allowed the admin-only membership.grant (reverted throwaway principal %q)", probe.principalID)
	}
	if refusalErr := publicRefusalError(grantErr, 403, api.CodeForbidden); refusalErr != nil {
		return fmt.Errorf("membership.grant with the viewer credential did not return 403 forbidden: %w (throwaway principal %q may remain if the grant committed)", refusalErr, probe.principalID)
	}

	document, err := refusalPipelineDocument()
	if err != nil {
		return fmt.Errorf("build the refusal pipeline document: %w", err)
	}
	startKey, err := probeIdempotencyKey("q02-viewer-start")
	if err != nil {
		return err
	}
	if _, err := probe.client.PostAlternate(ctx, viewerToken, "pipeline.start",
		api.PipelineStartRequest{ProjectID: probe.projectID, YAML: document}, startKey); err == nil {
		return fmt.Errorf("the viewer was allowed pipeline.start")
	} else if refusalErr := publicRefusalError(err, 403, api.CodeForbidden); refusalErr != nil {
		return fmt.Errorf("pipeline.start with the viewer credential did not return 403 forbidden: %w", refusalErr)
	}

	if err := probe.readExecutionList(ctx, probe.adminToken); err != nil {
		return fmt.Errorf("the admin's positive execution.list control was refused: %w", err)
	}
	return nil
}

// TestStageAuthorityRoleRefusal proves a lower-privilege actor that exists on
// the stage is refused the operations its role never grants. The viewer service
// is created through the public API, reads with its own credential, is refused
// membership.grant and pipeline.start with 403 forbidden (not 401), and the
// admin keeps a positive execution.list control. The service is disabled on
// every path; only the ADMIN persona is required.
func TestStageAuthorityRoleRefusal(t *testing.T) {
	stageWave1Parallel(t)
	config := loadStageConfig(t)
	requireStageInputs(t, config,
		InputAPIURL, InputCAPEM, InputTLSServerName,
		InputAdminToken, InputProjectID, InputEvidenceDir,
	)
	client := newStageClient(t, config)
	started := time.Now().UTC()

	principal, err := probeID("q02-authority-principal")
	if err != nil {
		t.Fatalf("generate the probe principal: %v", err)
	}
	probe := &roleAuthorityProbe{
		client:      client,
		adminToken:  config.AdminToken,
		projectID:   config.ProjectID,
		principalID: principal,
	}
	if err := probe.run(context.Background()); err != nil {
		t.Fatal(err)
	}

	finished := time.Now().UTC()
	writeStageEvidence(t, config, stageEvidence{
		Name:         "stage-authority-roles",
		Probe:        "TestStageAuthorityRoleRefusal",
		Started:      started,
		Finished:     finished,
		ResourceID:   probe.serviceID,
		ExtraSecrets: probe.issued,
	})
}
