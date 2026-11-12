//go:build qualification

package production

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
)

// serviceAuthorityProbe tracks one service account through the public lifecycle
// so a failure still revokes and disables what the probe created. The service
// identity, the last committed version and the issued credentials are the only
// facts it keeps.
type serviceAuthorityProbe struct {
	client    *API
	config    Config
	serviceID string
	version   uint64
	disabled  bool
	issued    []Secret
}

// cleanup best-effort revokes any surviving credential and disables the service
// at the last known version. It never logs a credential.
func (probe *serviceAuthorityProbe) cleanup() {
	if probe.serviceID == "" || probe.disabled {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if probe.version > 0 {
		if key, err := probeIdempotencyKey("q02-service-cleanup"); err == nil {
			result, err := probe.client.DeleteVersioned(ctx, probe.config.AdminToken, "service.credential.revoke",
				api.ServiceCredentialRevokeRequest{ServiceID: probe.serviceID}, key, probe.version)
			if err == nil {
				if revoked, decodeErr := decodeStageResult[api.ServiceCredentialRevokeResult](result, "service.credential.revoke"); decodeErr == nil {
					probe.version = revoked.Version
				}
			}
		}
		if key, err := probeIdempotencyKey("q02-service-cleanup"); err == nil {
			if _, err := probe.client.PostVersioned(ctx, probe.config.AdminToken, "service.disable",
				api.ServiceDisableRequest{ServiceID: probe.serviceID}, key, probe.version); err == nil {
				probe.disabled = true
			}
		}
	}
}

// evidence returns the allowlisted record of the completed lifecycle. Every
// credential the probe issued is registered with the writer.
func (probe *serviceAuthorityProbe) evidence(started, finished time.Time) stageEvidence {
	return stageEvidence{
		Name:         "stage-authority-service",
		Probe:        "TestStageAuthorityServiceLifecycle",
		Started:      started,
		Finished:     finished,
		ResourceID:   probe.serviceID,
		ExtraSecrets: append([]Secret(nil), probe.issued...),
	}
}

// readExecutionList performs the service account's permitted read and validates
// the result DTO.
func (probe *serviceAuthorityProbe) readExecutionList(ctx context.Context, token Secret) error {
	result, err := probe.client.Get(ctx, token, "execution.list",
		api.ExecutionListRequest{ProjectID: probe.config.ProjectID}, "")
	if err != nil {
		return err
	}
	_, err = decodeStageResult[api.ExecutionListResult](result, "execution.list")
	return err
}

// run drives the whole lifecycle: create, issue, use, rotate, revoke, reissue,
// disable. Every negative follows a positive control on the same credential,
// and every mutation carries the version returned by the previous one.
func (probe *serviceAuthorityProbe) run(ctx context.Context) error {
	serviceID, err := probeID("q02-authority-service")
	if err != nil {
		return fmt.Errorf("generate the probe service identity: %w", err)
	}
	createKey, err := probeIdempotencyKey("q02-service-create")
	if err != nil {
		return err
	}
	createResult, err := probe.client.Post(ctx, probe.config.AdminToken, "service.create",
		api.ServiceCreateRequest{ProjectID: probe.config.ProjectID, ServiceID: serviceID, Roles: []string{"viewer"}}, createKey)
	if err != nil {
		return fmt.Errorf("service.create was refused: %w", err)
	}
	created, err := decodeStageResult[api.ServiceCreateResult](createResult, "service.create")
	if err != nil {
		return fmt.Errorf("service.create returned an invalid result: %w", err)
	}
	if created.ServiceID != serviceID {
		return fmt.Errorf("service.create returned service %q, want %q", created.ServiceID, serviceID)
	}
	probe.serviceID = serviceID
	probe.version = created.Version

	issueKey, err := probeIdempotencyKey("q02-service-issue")
	if err != nil {
		return err
	}
	issueResult, err := probe.client.PostVersioned(ctx, probe.config.AdminToken, "service.credential.issue",
		api.ServiceCredentialIssueRequest{ServiceID: serviceID, TTLSeconds: 900}, issueKey, probe.version)
	if err != nil {
		return fmt.Errorf("service.credential.issue was refused: %w", err)
	}
	issued, err := decodeStageResult[api.ServiceCredentialIssueResult](issueResult, "service.credential.issue")
	if err != nil {
		return fmt.Errorf("service.credential.issue returned an invalid result: %w", err)
	}
	first, err := stageCredential(issued.Token, serviceCredentialPrefix)
	if err != nil {
		return fmt.Errorf("service.credential.issue: %w", err)
	}
	probe.version = issued.Version
	probe.issued = append(probe.issued, first)
	if err := probe.readExecutionList(ctx, first); err != nil {
		return fmt.Errorf("the issued service credential's positive execution.list was refused: %w", err)
	}

	rotateKey, err := probeIdempotencyKey("q02-service-rotate")
	if err != nil {
		return err
	}
	rotateResult, err := probe.client.PostVersioned(ctx, probe.config.AdminToken, "service.credential.rotate",
		api.ServiceCredentialRotateRequest{ServiceID: serviceID, TTLSeconds: 900}, rotateKey, probe.version)
	if err != nil {
		return fmt.Errorf("service.credential.rotate was refused: %w", err)
	}
	rotated, err := decodeStageResult[api.ServiceCredentialRotateResult](rotateResult, "service.credential.rotate")
	if err != nil {
		return fmt.Errorf("service.credential.rotate returned an invalid result: %w", err)
	}
	second, err := stageCredential(rotated.Token, serviceCredentialPrefix)
	if err != nil {
		return fmt.Errorf("service.credential.rotate: %w", err)
	}
	probe.version = rotated.Version
	probe.issued = append(probe.issued, second)
	if first.Reveal() == second.Reveal() {
		return fmt.Errorf("service.credential.rotate returned the same credential as the issue")
	}
	if err := probe.readExecutionList(ctx, second); err != nil {
		return fmt.Errorf("the rotated service credential's positive execution.list was refused: %w", err)
	}
	if err := publicRefusalError(probe.readExecutionList(ctx, first), 401, api.CodeUnauthenticated); err != nil {
		return fmt.Errorf("the rotated-away credential: %w", err)
	}

	revokeKey, err := probeIdempotencyKey("q02-service-revoke")
	if err != nil {
		return err
	}
	revokeResult, err := probe.client.DeleteVersioned(ctx, probe.config.AdminToken, "service.credential.revoke",
		api.ServiceCredentialRevokeRequest{ServiceID: serviceID}, revokeKey, probe.version)
	if err != nil {
		return fmt.Errorf("service.credential.revoke was refused: %w", err)
	}
	revoked, err := decodeStageResult[api.ServiceCredentialRevokeResult](revokeResult, "service.credential.revoke")
	if err != nil {
		return fmt.Errorf("service.credential.revoke returned an invalid result: %w", err)
	}
	if revoked.Invalidated == nil || !*revoked.Invalidated {
		return fmt.Errorf("service.credential.revoke reported no invalidation")
	}
	probe.version = revoked.Version
	if err := publicRefusalError(probe.readExecutionList(ctx, second), 401, api.CodeUnauthenticated); err != nil {
		return fmt.Errorf("the revoked credential: %w", err)
	}

	reissueKey, err := probeIdempotencyKey("q02-service-reissue")
	if err != nil {
		return err
	}
	reissueResult, err := probe.client.PostVersioned(ctx, probe.config.AdminToken, "service.credential.issue",
		api.ServiceCredentialIssueRequest{ServiceID: serviceID, TTLSeconds: 900}, reissueKey, probe.version)
	if err != nil {
		return fmt.Errorf("the second service.credential.issue was refused: %w", err)
	}
	reissued, err := decodeStageResult[api.ServiceCredentialIssueResult](reissueResult, "service.credential.issue")
	if err != nil {
		return fmt.Errorf("the second service.credential.issue returned an invalid result: %w", err)
	}
	third, err := stageCredential(reissued.Token, serviceCredentialPrefix)
	if err != nil {
		return fmt.Errorf("the second service.credential.issue: %w", err)
	}
	probe.version = reissued.Version
	probe.issued = append(probe.issued, third)
	if err := probe.readExecutionList(ctx, third); err != nil {
		return fmt.Errorf("the reissued service credential's positive execution.list was refused: %w", err)
	}

	disableKey, err := probeIdempotencyKey("q02-service-disable")
	if err != nil {
		return err
	}
	disableResult, err := probe.client.PostVersioned(ctx, probe.config.AdminToken, "service.disable",
		api.ServiceDisableRequest{ServiceID: serviceID}, disableKey, probe.version)
	if err != nil {
		return fmt.Errorf("service.disable was refused: %w", err)
	}
	disabled, err := decodeStageResult[api.ServiceDisableResult](disableResult, "service.disable")
	if err != nil {
		return fmt.Errorf("service.disable returned an invalid result: %w", err)
	}
	if disabled.RevokedCredentials == 0 {
		return fmt.Errorf("service.disable reported no revoked credential")
	}
	probe.version = disabled.Version
	probe.disabled = true
	if err := publicRefusalError(probe.readExecutionList(ctx, third), 401, api.CodeUnauthenticated); err != nil {
		return fmt.Errorf("the disabled service's credential: %w", err)
	}
	return nil
}

// TestStageAuthorityServiceLifecycle proves the public service-account
// lifecycle against the live stage. The cleanup is registered before the first
// mutation so a mid-probe failure still revokes and disables; the evidence
// registers every issued credential with the writer.
func TestStageAuthorityServiceLifecycle(t *testing.T) {
	stageWave1Parallel(t)
	config := loadStageConfig(t)
	requireStageInputs(t, config,
		InputAPIURL, InputCAPEM, InputTLSServerName,
		InputAdminToken, InputProjectID, InputEvidenceDir,
	)
	client := newStageClient(t, config)
	probe := &serviceAuthorityProbe{client: client, config: config}
	t.Cleanup(probe.cleanup)
	started := time.Now().UTC()
	if err := probe.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	writeStageEvidence(t, config, probe.evidence(started, time.Now().UTC()))
}
