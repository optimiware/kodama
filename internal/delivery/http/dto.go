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
		Id: httpapi.ID(rec.ID), Name: rec.Name, Kind: EnvironmentKind(rec.Kind), Target: rec.Target,
		Secrets: credentialsDTO(rec.Secrets), Config: rec.Config,
		Version: httpapi.Version(rec.Version), CreatedAt: rec.CreatedAt.UTC(), UpdatedAt: rec.UpdatedAt.UTC(),
	}
	if out.Config == nil {
		out.Config = map[string]string{}
	}
	out.Namespace = rec.Namespace
	if rec.Kubeconfig.Name != "" {
		out.Kubeconfig = credentialDTO(rec.Kubeconfig)
	}
	if rec.TargetProfile != "" {
		profile, digest, provider := rec.TargetProfile, Digest(rec.TargetProfileDigest), rec.TargetProvider
		out.TargetProfile, out.TargetProfileDigest, out.TargetProvider = &profile, &digest, &provider
		kinds := make([]ArtifactKind, 0, len(rec.TargetArtifactKinds))
		for _, k := range rec.TargetArtifactKinds {
			kinds = append(kinds, ArtifactKind(k))
		}
		out.TargetArtifactKinds = &kinds
		egress := append([]string(nil), rec.TargetEgress...)
		out.TargetEgress = &egress
		cfg := cloneStringMap(rec.TargetConfig)
		out.TargetConfig = &cfg
	}
	if rec.Kind == delivery.KindPreview {
		ttl := int(rec.PreviewTTL / time.Second)
		out.PreviewTtlSeconds = &ttl
	}
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func environmentInput(in EnvironmentCreate) delivery.EnvironmentInput {
	out := delivery.EnvironmentInput{Name: in.Name, Kind: delivery.EnvironmentKind(in.Kind)}
	if in.Target != nil {
		out.Target = *in.Target
	}
	if in.Namespace != nil {
		out.Namespace = *in.Namespace
	}
	if in.Kubeconfig != nil {
		out.Kubeconfig = delivery.Credential{Name: in.Kubeconfig.Name, Ref: in.Kubeconfig.Ref, Delivery: runtimev1.SecretDelivery(in.Kubeconfig.Delivery)}
	}
	if in.TargetProfile != nil {
		out.TargetProfile = *in.TargetProfile
	}
	if in.TargetConfig != nil {
		out.TargetConfig = *in.TargetConfig
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
	out.TargetProfile = in.TargetProfile
	if in.TargetProfileDigest != nil {
		d := string(*in.TargetProfileDigest)
		out.TargetProfileDigest = &d
	}
	if in.TargetConfig != nil {
		c := *in.TargetConfig
		out.TargetConfig = &c
	}
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

func resourcesDTO(in []delivery.ResourceRef) []ResourceRef {
	out := make([]ResourceRef, 0, len(in))
	for _, r := range in {
		dto := ResourceRef{Provider: r.Provider, Type: r.Type, Id: r.ID}
		if r.Name != "" {
			n := r.Name
			dto.Name = &n
		}
		if r.Version != "" {
			v := r.Version
			dto.Version = &v
		}
		if r.ETag != "" {
			e := r.ETag
			dto.Etag = &e
		}
		if r.URL != "" {
			u := r.URL
			dto.Url = &u
		}
		if r.Ready {
			ready := true
			dto.Ready = &ready
		}
		if r.Metadata != nil {
			m := cloneStringMap(r.Metadata)
			dto.Metadata = &m
		}
		out = append(out, dto)
	}
	return out
}

func resourcesFrom(in *[]ResourceRef) []delivery.ResourceRef {
	if in == nil {
		return nil
	}
	out := make([]delivery.ResourceRef, 0, len(*in))
	for _, r := range *in {
		ref := delivery.ResourceRef{Provider: r.Provider, Type: r.Type, ID: r.Id}
		if r.Name != nil {
			ref.Name = *r.Name
		}
		if r.Version != nil {
			ref.Version = *r.Version
		}
		if r.Etag != nil {
			ref.ETag = *r.Etag
		}
		if r.Url != nil {
			ref.URL = *r.Url
		}
		if r.Ready != nil {
			ref.Ready = *r.Ready
		}
		if r.Metadata != nil {
			ref.Metadata = cloneStringMap(*r.Metadata)
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
	img := rec.Image
	artifact := rec.Artifact
	if artifact.Kind != "" {
		img = delivery.Image{ArtifactID: artifact.ArtifactID, Digest: artifact.Digest}
	}
	var artifactDTO *Artifact
	if artifact.Kind != "" {
		media := artifact.MediaType
		a := Artifact{Kind: ArtifactKind(artifact.Kind), ArtifactId: httpapi.ID(artifact.ArtifactID), Digest: Digest(artifact.Digest)}
		if media != "" {
			a.MediaType = &media
		}
		artifactDTO = &a
	}
	resources := resourcesDTO(rec.Resources)
	return Deployment{
		Id: string(rec.ID), EnvironmentId: httpapi.ID(rec.Environment), ConfigurationDigest: string(rec.ConfigDigest), Namespace: rec.Namespace,
		Source:   Source{RepositoryUrl: rec.Source.RepositoryURL, Commit: rec.Source.Commit},
		Image:    Image{ArtifactId: httpapi.ID(img.ArtifactID), Digest: string(img.Digest)},
		Artifact: artifactDTO, Resources: &resources,
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

func registrationRequest(src Source, img Image, artifact *Artifact) delivery.RegistrationRequest {
	out := delivery.RegistrationRequest{Source: delivery.Source{RepositoryURL: src.RepositoryUrl, Commit: src.Commit}}
	out.Image = delivery.Image{ArtifactID: domain.ArtifactID(img.ArtifactId), Digest: runtimev1.Digest(img.Digest)}
	if artifact != nil {
		out.Artifact = delivery.DeployableArtifact{Kind: delivery.ArtifactKind(artifact.Kind), ArtifactID: domain.ArtifactID(artifact.ArtifactId),
			Digest: runtimev1.Digest(artifact.Digest)}
		if artifact.MediaType != nil {
			out.Artifact.MediaType = *artifact.MediaType
		}
	}
	return out
}

func outcomeReport(in OutcomeReport) delivery.OutcomeReport {
	out := delivery.OutcomeReport{State: delivery.OutcomeState(in.State), Objects: objectsFrom(in.Objects), Resources: resourcesFrom(in.Resources)}
	if in.Reason != nil {
		out.Reason = *in.Reason
	}
	return out
}

func cleanupReport(in CleanupReport) delivery.CleanupReport {
	out := delivery.CleanupReport{State: delivery.CleanupState(in.State), Deleted: objectsFrom(in.Deleted), DeletedResources: resourcesFrom(in.DeletedResources)}
	if in.Reason != nil {
		out.Reason = *in.Reason
	}
	return out
}
