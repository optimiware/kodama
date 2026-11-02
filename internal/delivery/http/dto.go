package deliveryhttp

import (
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
)

// The wire types are generated from api/delivery.yaml. This file maps them to
// and from the use cases' values. No aggregate is serialized directly: a field
// reaches the wire only by being listed here, so adding one to an entity cannot
// leak it, and nothing here reads a credential value (the module holds none).

func credentialDTO(c delivery.Credential) CredentialBinding {
	return CredentialBinding{Name: c.Name, Ref: c.Ref, Delivery: SecretDelivery(c.Delivery)}
}

func credentialsDTO(in []delivery.Credential) []CredentialBinding {
	out := make([]CredentialBinding, 0, len(in))
	for _, c := range in {
		out = append(out, credentialDTO(c))
	}
	return out
}

func credentialsFrom(in []CredentialBinding) []delivery.Credential {
	out := make([]delivery.Credential, 0, len(in))
	for _, c := range in {
		out = append(out, delivery.Credential{Name: c.Name, Ref: c.Ref, Delivery: runtimev1.SecretDelivery(c.Delivery)})
	}
	return out
}

func environmentDTO(e delivery.Environment) Environment {
	rec := e.Record()
	out := Environment{
		Id: httpapi.ID(rec.ID), Name: rec.Name, Kind: EnvironmentKind(rec.Kind), Target: rec.Target, Namespace: rec.Namespace,
		Kubeconfig: credentialDTO(rec.Kubeconfig), Secrets: credentialsDTO(rec.Secrets), Config: rec.Config,
		Version: httpapi.Version(rec.Version), CreatedAt: rec.CreatedAt.UTC(), UpdatedAt: rec.UpdatedAt.UTC(),
	}
	if out.Config == nil {
		out.Config = map[string]string{}
	}
	if rec.Kind == delivery.KindPreview {
		ttl := int(rec.PreviewTTL / time.Second)
		out.PreviewTtlSeconds = &ttl
	}
	return out
}

func environmentInput(in EnvironmentCreate) delivery.EnvironmentInput {
	out := delivery.EnvironmentInput{
		Name: in.Name, Kind: delivery.EnvironmentKind(in.Kind), Target: in.Target, Namespace: in.Namespace,
		Kubeconfig: delivery.Credential{Name: in.Kubeconfig.Name, Ref: in.Kubeconfig.Ref, Delivery: runtimev1.SecretDelivery(in.Kubeconfig.Delivery)},
	}
	if in.Secrets != nil {
		out.Secrets = credentialsFrom(*in.Secrets)
	}
	if in.Config != nil {
		out.Config = *in.Config
	}
	if in.PreviewTtlSeconds != nil {
		ttl := time.Duration(*in.PreviewTtlSeconds) * time.Second
		out.PreviewTTL = &ttl
	}
	return out
}

func environmentEdit(in EnvironmentEdit) delivery.EnvironmentEdit {
	var out delivery.EnvironmentEdit
	out.Target, out.Namespace = in.Target, in.Namespace
	if in.Kubeconfig != nil {
		c := delivery.Credential{Name: in.Kubeconfig.Name, Ref: in.Kubeconfig.Ref, Delivery: runtimev1.SecretDelivery(in.Kubeconfig.Delivery)}
		out.Kubeconfig = &c
	}
	if in.Secrets != nil {
		s := credentialsFrom(*in.Secrets)
		out.Secrets = &s
	}
	if in.Config != nil {
		c := *in.Config
		out.Config = &c
	}
	if in.PreviewTtlSeconds != nil {
		ttl := time.Duration(*in.PreviewTtlSeconds) * time.Second
		out.PreviewTTL = &ttl
	}
	return out
}

func objectsDTO(in []delivery.ObjectRef) []ObjectRef {
	out := make([]ObjectRef, 0, len(in))
	for _, o := range in {
		dto := ObjectRef{ApiVersion: o.APIVersion, Kind: o.Kind, Namespace: o.Namespace, Name: o.Name}
		if o.UID != "" {
			uid := o.UID
			dto.Uid = &uid
		}
		if o.Ready {
			ready := true
			dto.Ready = &ready
		}
		out = append(out, dto)
	}
	return out
}

func objectsFrom(in *[]ObjectRef) []delivery.ObjectRef {
	if in == nil {
		return nil
	}
	out := make([]delivery.ObjectRef, 0, len(*in))
	for _, o := range *in {
		ref := delivery.ObjectRef{APIVersion: o.ApiVersion, Kind: o.Kind, Namespace: o.Namespace, Name: o.Name}
		if o.Uid != nil {
			ref.UID = *o.Uid
		}
		if o.Ready != nil {
			ref.Ready = *o.Ready
		}
		out = append(out, ref)
	}
	return out
}

func timePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func deploymentDTO(d delivery.Deployment) Deployment {
	rec := d.Record()
	state := CleanupState("none")
	if rec.CleanupState != delivery.CleanupNone {
		state = CleanupState(rec.CleanupState)
	}
	cleanup := Cleanup{State: state, RequestedAt: timePtr(rec.CleanupRequestedAt)}
	if rec.CleanupReason != "" {
		r := rec.CleanupReason
		cleanup.Reason = &r
	}
	source, sourceSet := sourceDTO(rec.Source)
	return Deployment{
		Id: string(rec.ID), EnvironmentId: httpapi.ID(rec.Environment), ConfigurationDigest: string(rec.ConfigDigest), Namespace: rec.Namespace,
		Source: source, SourceSet: sourceSet,
		Image: Image{ArtifactId: httpapi.ID(rec.Image.ArtifactID), Digest: string(rec.Image.Digest)},
		Producer: Producer{AttemptId: httpapi.ID(rec.Producer.Attempt), RunId: httpapi.ID(rec.Producer.Run), StepId: rec.Producer.Step,
			ToolImage: rec.Producer.ToolImage},
		RunId: httpapi.ID(rec.Run), StepId: rec.Step, ActionKey: rec.ActionKey, AttemptId: httpapi.ID(rec.Attempt),
		Status: DeploymentStatus(rec.Status), Reason: rec.Reason, OutcomeUnknown: d.OutcomeUnknown(),
		Objects: objectsDTO(rec.Objects), ExpiresAt: timePtr(rec.ExpiresAt), Cleanup: cleanup, DeletedAt: timePtr(rec.DeletedAt),
		Version: httpapi.Version(rec.Version), CreatedAt: rec.CreatedAt.UTC(), UpdatedAt: rec.UpdatedAt.UTC(),
	}
}

func receiptDTO(r delivery.Receipt, created bool) DeploymentReceipt {
	ann := make(map[string]string, len(r.Annotations))
	for k, v := range r.Annotations {
		ann[k] = v
	}
	return DeploymentReceipt{Deployment: deploymentDTO(r.Deployment), Annotations: ann, Created: created}
}

// registrationRequest converts a registration body: exactly one of source and
// sourceSet names the candidate. A malformed source set is a bad request, not a
// provenance refusal.
func registrationRequest(src *Source, set *DeploymentSourceSet, img Image) (delivery.RegistrationRequest, error) {
	out := delivery.RegistrationRequest{Image: delivery.Image{ArtifactID: domain.ArtifactID(img.ArtifactId), Digest: runtimev1.Digest(img.Digest)}}
	switch {
	case (src == nil) == (set == nil):
		return out, httpapi.BadRequestError("exactly one of source and sourceSet names the candidate")
	case src != nil:
		out.Source = delivery.Source{RepositoryURL: src.RepositoryUrl, Commit: src.Commit}
	default:
		commits := make([]delivery.SourceCommit, len(set.Commits))
		for i, c := range set.Commits {
			commits[i] = delivery.SourceCommit{RepositoryID: string(c.RepositoryId), Commit: c.Commit}
		}
		ss, err := delivery.NewSourceSet(set.Digest, commits)
		if err != nil {
			return out, httpapi.BadRequestError("the source set is not valid")
		}
		// A set is canonical: ordered by repository, so one candidate has one body.
		for i, c := range ss.Commits() {
			if set.Commits[i].RepositoryId != httpapi.ID(c.RepositoryID) {
				return out, httpapi.BadRequestError("the source set is ordered by repository")
			}
		}
		out.Source = delivery.Source{SourceSet: ss}
	}
	return out, nil
}

// sourceDTO renders the source of a deployment, always a repository and commit
// (the primary source's for a source set), and the source set beside it when
// there is one (an additive member).
func sourceDTO(s delivery.Source) (Source, *DeploymentSourceSet) {
	if s.SourceSet.IsZero() {
		return Source{RepositoryUrl: s.RepositoryURL, Commit: s.Commit}, nil
	}
	out := &DeploymentSourceSet{Digest: s.SourceSet.Digest}
	for _, c := range s.SourceSet.Commits() {
		out.Commits = append(out.Commits, DeploymentSourceCommit{RepositoryId: httpapi.ID(c.RepositoryID), Commit: c.Commit})
	}
	return Source{RepositoryUrl: s.RepositoryURL, Commit: s.Commit}, out
}

func outcomeReport(in OutcomeReport) delivery.OutcomeReport {
	out := delivery.OutcomeReport{State: delivery.OutcomeState(in.State), Objects: objectsFrom(in.Objects)}
	if in.Reason != nil {
		out.Reason = *in.Reason
	}
	return out
}

func cleanupReport(in CleanupReport) delivery.CleanupReport {
	out := delivery.CleanupReport{State: delivery.CleanupState(in.State), Deleted: objectsFrom(in.Deleted)}
	if in.Reason != nil {
		out.Reason = *in.Reason
	}
	return out
}
