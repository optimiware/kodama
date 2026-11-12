package opsapp

import (
	"context"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/shell/event"
	shellrequest "github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/transport/ops"
	"github.com/optimiware/kodama/internal/shell/verify"
)

// verifyHandlers binds actor verification and verified resource reports.
func verifyHandlers(deps Dependencies) map[authorizeOperation]ops.Handler {
	return map[authorizeOperation]ops.Handler{
		"actor.verify": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.VerifyRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			approval, err := parseApproval(request.Approval)
			if err != nil {
				return api.Result{}, err
			}
			// The action identity is the actor's CURRENT running container
			// action, resolved server-side from the forwarded credential. It is
			// the deadline authority-read target: the public request never names
			// an action and a client-supplied value is not representable. A
			// human or service actor resolves to no action, while an execution
			// actor with no running container action is refused here, before any
			// verification record.
			actionID, err := deps.ActorActions.ResolveActorAction(ctx, request.ActorCredential)
			if err != nil {
				return api.Result{}, err
			}
			record, err := deps.Verifier.Verify(ctx, verify.Attempt{
				Scope:      call.Scope,
				Service:    call.Principal,
				Credential: request.ActorCredential,
				ResourceID: resource.ResourceID(request.ResourceID),
				Operation:  resource.Operation(request.Operation),
				Subject:    verifySubject(request.Subject),
				Approval:   approval,
				ActionID:   actionID,
			})
			if err != nil {
				return api.Result{}, err
			}
			return respond(call, api.VerifyResponse{
				Principal: api.VerifiedPrincipal{
					ID:   string(record.Actor.ID),
					Kind: string(record.Actor.Kind),
				},
				Roles:          roleNames(record.Actor.Roles),
				Approval:       record.Approved,
				VerificationID: string(record.ID),
			})
		}),
		"resource.report": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.ReportRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			committed, err := deps.Reports.Report(ctx, verify.ReportCommand{
				Scope:          call.Scope,
				Service:        call.Principal,
				SourceEventID:  request.SourceEventID,
				EventType:      event.ReportType(request.EventType),
				VerificationID: verify.VerificationID(request.VerificationID),
				ResourceID:     resource.ResourceID(request.ResourceID),
				Operation:      resource.Operation(request.Operation),
				Subject:        verifySubject(request.Subject),
			})
			if err != nil {
				return api.Result{}, err
			}
			return respond(call, api.ReportResponse{
				VerificationID: string(committed.VerificationID),
				EventID:        string(committed.EventID),
				Replayed:       committed.Replayed,
			})
		}),
	}
}

// verifySubject maps the public typed subject onto the verification subject.
func verifySubject(subject api.Subject) verify.Subject {
	fields := make([]verify.SubjectField, 0, len(subject.Fields))
	for _, field := range subject.Fields {
		fields = append(fields, verify.SubjectField{
			Name:  field.Name,
			Kind:  verify.ValueKind(field.Kind),
			Value: field.Value,
		})
	}
	return verify.Subject{Fields: fields}
}

// parseApproval parses the optional role assignment of an approval.
func parseApproval(value string) (shellrequest.Assignment, error) {
	if value == "" {
		return shellrequest.Assignment{}, nil
	}
	return shellrequest.ParseAssignment(value)
}
