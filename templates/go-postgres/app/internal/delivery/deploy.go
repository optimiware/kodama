package delivery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Annotation names the factory puts on every receipt. Names are installation
// configuration; these are the published defaults.
const (
	AnnotationDeploymentID = "kodama.optimi.com/deployment-id"
	AnnotationActionKey    = "kodama.optimi.com/action-key"
	AnnotationConfigDigest = "kodama.optimi.com/configuration-digest"
)

// Factory is the consumer-owned port to the factory's runtime callbacks.
type Factory interface {
	Context(ctx context.Context) (Context, error)
	Register(ctx context.Context, src Source, img Image) (Receipt, error)
	Lookup(ctx context.Context, actionKey string) (Receipt, error)
	Outcome(ctx context.Context, deploymentID string, rep Report) (Receipt, error)
}

var _ Factory = (*Runtime)(nil)

// ImageChecker confirms the registry holds the image digest.
type ImageChecker interface {
	Digest(ctx context.Context, repo, reference string) (string, error)
}

// Input is the explicit configuration of one deployment run.
type Input struct {
	// Kind is "preview" or "stage".
	Kind string
	// SnapshotJSON is the nonsecret environment snapshot input.
	SnapshotJSON string
	SecretsDir   string
	Getenv       func(string) string
	OutputsDir   string
	// ClusterFor builds the namespace-scoped cluster adapter from the delivered kubeconfig path.
	ClusterFor       func(kubeconfigPath string) Cluster
	Images           ImageChecker
	ReadinessTimeout time.Duration
	PollInterval     time.Duration
}

// Result is the outcome the script reports through its results manifest.
type Result struct {
	Completed    bool
	Reason       string
	DeploymentID string
	AppURL       string
	Evidence     any
}

// Deployer applies one deployment.
type Deployer struct {
	Factory Factory
	In      Input
}

// Failure reasons (entrypoint reason codes).
const (
	ReasonSnapshot       = "snapshot_mismatch"
	ReasonConfig         = "config_incomplete"
	ReasonImage          = "image_unverified"
	ReasonAuthority      = "authority_lost"
	ReasonCallback       = "callback_unavailable"
	ReasonReceipt        = "receipt_mismatch"
	ReasonForeign        = "foreign_object"
	ReasonIntent         = "intent_conflict"
	ReasonApply          = "apply_not_observed"
	ReasonPartial        = "partial_apply"
	ReasonRollout        = "rollout_timeout"
	ReasonMigration      = "migration_failed"
	ReasonUnreported     = "outcome_unreported"
	ReasonObservation    = "observation_failed"
	ReasonEarlierFailure = "deployment_failed_earlier"
	ReasonNotLive        = "settled_state_not_live"
)

type stop struct{ reason, detail string }

func (s stop) Error() string { return s.reason + ": " + s.detail }

func halt(reason, f string, a ...any) error { return stop{reason, fmt.Sprintf(f, a...)} }

// Run performs the deployment. A returned Result with Completed=false is a
// reportable failure; an error is an internal problem (the script exits 1).
func (d *Deployer) Run(ctx context.Context) (Result, error) {
	res, err := d.run(ctx)
	var s stop
	if errors.As(err, &s) {
		res.Completed, res.Reason = false, s.reason
		return res, nil
	}
	return res, err
}

type state struct {
	d      *Deployer
	cx     Context
	snap   Snapshot
	plan   Plan
	cl     Cluster
	rec    Record
	annots map[string]string
}

func (d *Deployer) run(ctx context.Context) (Result, error) {
	in := d.In
	if in.PollInterval <= 0 {
		in.PollInterval = 2 * time.Second
	}
	if in.ReadinessTimeout <= 0 {
		in.ReadinessTimeout = 3 * time.Minute
	}
	d.In = in
	if in.Kind != "preview" && in.Kind != "stage" {
		return Result{}, errors.New("delivery: kind must be preview or stage")
	}
	cx, err := d.Factory.Context(ctx)
	if err != nil {
		return Result{}, d.classify(err, "reading the pinned context")
	}
	if cx.Workspace.Source == nil || cx.ActionKey == "" || cx.Environment == nil {
		return Result{}, halt(ReasonSnapshot, "the attempt has no pinned source, action key or environment")
	}
	snap, err := ParseSnapshot(in.SnapshotJSON)
	if err == nil {
		err = snap.Verify(cx, in.Kind)
	}
	if err != nil {
		return Result{}, halt(ReasonSnapshot, "%v", err)
	}

	repo, _ := cx.Input("image_repository")
	digest, _ := cx.Input("image_digest")
	artifact, _ := cx.Input("image")
	if artifact == "" || !ValidDigest(digest) || !ValidRepository(repo) {
		return Result{}, halt(ReasonImage, "the pinned inputs image, image_digest and image_repository are required")
	}
	if got, err := in.Images.Digest(ctx, repo, digest); err != nil || got != digest {
		return Result{}, halt(ReasonImage, "the registry does not hold %s@%s", repo, digest)
	}

	secrets, kubeconfig, err := d.credentials(snap)
	if err != nil {
		return Result{}, halt(ReasonConfig, "%v", err)
	}
	plan := Plan{Namespace: snap.Namespace, ImageRepo: repo, ImageDigest: digest, SourceCommit: cx.Workspace.Source.Commit, ConfigDigest: cx.Environment.ConfigDigest, Config: snap.Config, Secrets: secrets}
	if err := plan.Validate(); err != nil {
		return Result{}, halt(ReasonConfig, "%v", err)
	}

	// The durable record exists before any external change.
	receipt, err := d.Factory.Register(ctx, *cx.Workspace.Source, Image{ArtifactID: artifact, Digest: digest})
	if err != nil {
		return Result{}, d.classify(err, "registering the deployment")
	}
	rec := receipt.Deployment
	if rec.EnvironmentID != cx.Environment.ID || rec.ConfigurationDigest != cx.Environment.ConfigDigest || rec.Namespace != snap.Namespace ||
		rec.ActionKey != cx.ActionKey || rec.Source != *cx.Workspace.Source || rec.Image.Digest != digest || rec.Image.ArtifactID != artifact ||
		receipt.Annotations[AnnotationDeploymentID] != rec.ID || receipt.Annotations[AnnotationActionKey] != cx.ActionKey ||
		receipt.Annotations[AnnotationConfigDigest] != cx.Environment.ConfigDigest || rec.ID == "" {
		return Result{}, halt(ReasonReceipt, "the factory's deployment record does not match the pinned context")
	}
	plan.DeploymentID, plan.Annotations = rec.ID, receipt.Annotations
	s := &state{d: d, cx: cx, snap: snap, plan: plan, cl: in.ClusterFor(kubeconfig), rec: rec, annots: receipt.Annotations}

	out := Result{DeploymentID: rec.ID, AppURL: appURL(snap)}
	if err := s.drive(ctx); err != nil {
		return out, err
	}
	out.Completed = true
	out.Evidence = s.evidence()
	return out, nil
}

func appURL(snap Snapshot) string {
	if u := strings.TrimSpace(snap.Config["APP_URL"]); u != "" {
		return u
	}
	return "http://inventory-api." + snap.Namespace + ".svc.cluster.local:8080"
}

// credentials reads the delivered script credentials. The deployment credential
// is a file; others follow their binding's delivery form. Values are never logged.
func (d *Deployer) credentials(snap Snapshot) (map[string]string, string, error) {
	out := map[string]string{}
	kube := ""
	for _, b := range snap.Secrets {
		var v string
		switch b.Delivery {
		case "file":
			path := filepath.Join(d.In.SecretsDir, b.Name)
			if filepath.Base(b.Name) != b.Name {
				return nil, "", fmt.Errorf("credential %q has an unsafe name", b.Name)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return nil, "", fmt.Errorf("credential %q is not delivered", b.Name)
			}
			v = strings.TrimRight(string(raw), "\r\n")
			if b.Name == snap.Kubeconfig {
				kube = path
			}
		case "env":
			v = d.In.Getenv(b.Name)
		}
		if b.Name == snap.Kubeconfig {
			continue
		}
		out[b.Name] = v
	}
	if kube == "" {
		return nil, "", fmt.Errorf("the deployment credential %q must be delivered as a file", snap.Kubeconfig)
	}
	return out, kube, nil
}

// classify maps callback errors to a reportable stop. Authority loss and
// unavailability both stop before any effect that has not been recorded.
func (d *Deployer) classify(err error, what string) error {
	switch {
	case errors.Is(err, ErrAuthorityLost):
		return halt(ReasonAuthority, "%s: authority was refused or revoked; stopping without effects", what)
	case errors.Is(err, ErrUnavailable):
		return halt(ReasonCallback, "%s: the factory could not be reached", what)
	case errors.Is(err, ErrConflict):
		return halt(ReasonIntent, "%s: %v", what, err)
	}
	return err
}

// owned reports whether an observed object carries this deployment's identity:
// the deployment, action and configuration annotations must all equal the
// receipt's. An object that is merely ready but names another deployment,
// action or configuration is foreign and never produces success evidence.
func (s *state) owned(o Observed) bool {
	for _, k := range []string{AnnotationDeploymentID, AnnotationActionKey, AnnotationConfigDigest} {
		if want := s.annots[k]; want == "" || o.Annotations[k] != want {
			return false
		}
	}
	return true
}

func (s *state) refs() []ObjectRef { return s.plan.Refs() }

func (s *state) intended() []Object {
	var objs []Object
	for _, r := range s.refs() {
		objs = append(objs, r.object())
	}
	return objs
}

func (s *state) drive(ctx context.Context) error {
	switch {
	case s.rec.Status == "succeeded":
		// Already settled: confirm the live objects, never apply again.
		obs, err := s.observeAll(ctx)
		if err != nil {
			return halt(ReasonObservation, "%v", err)
		}
		for _, o := range obs {
			if !s.owned(o) {
				return halt(ReasonForeign, "%s/%s is ready but is not owned by this deployment", o.Ref.Kind, o.Ref.Name)
			}
		}
		if !allPresentReady(obs, len(s.refs())) {
			return halt(ReasonNotLive, "the deployment is recorded as succeeded but its objects are not all live and ready")
		}
		s.rec.Objects = reportObjects(obs)
		return nil
	case s.rec.Status == "failed":
		return halt(ReasonEarlierFailure, "the deployment was settled as failed (%s)", s.rec.Reason)
	case s.rec.OutcomeUnknown || s.rec.Reason == "outcome_unknown":
		// The previous apply may have taken effect: observe and settle, never reapply.
		return s.settle(ctx)
	}

	// Foreign objects are never adopted or modified.
	obs, err := s.observeAll(ctx)
	if err != nil {
		return halt(ReasonObservation, "%v", err)
	}
	for _, o := range obs {
		if !s.owned(o) {
			s.report(ctx, Report{State: "failed", Reason: ReasonForeign})
			return halt(ReasonForeign, "%s/%s exists and is not owned by this deployment", o.Ref.Kind, o.Ref.Name)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// The intent is recorded before the first external change.
	if _, err := s.d.Factory.Outcome(ctx, s.rec.ID, Report{State: "applying", Objects: s.intended()}); err != nil {
		if errors.Is(err, ErrOutcomeUnknown) {
			return s.settle(ctx)
		}
		return s.d.classify(err, "recording the apply intent")
	}
	list, err := s.plan.Render()
	if err != nil {
		s.report(ctx, Report{State: "failed", Reason: ReasonConfig})
		return halt(ReasonConfig, "%v", err)
	}
	if err := s.cl.Apply(ctx, s.plan.Namespace, list); err != nil {
		// The apply may have taken effect before the error: say so, then observe.
		s.report(ctx, Report{State: "unknown"})
		return s.settle(ctx)
	}
	return s.awaitAndSucceed(ctx)
}

func (s *state) report(ctx context.Context, rep Report) {
	// Best effort: a lost report is reconciled by the next attempt's lookup.
	_, _ = s.d.Factory.Outcome(context.WithoutCancel(ctx), s.rec.ID, rep)
}

func (s *state) observeAll(ctx context.Context) ([]Observed, error) {
	var out []Observed
	for _, r := range s.refs() {
		o, ok, err := s.cl.Get(ctx, r)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, o)
		}
	}
	return out, nil
}

func allPresentReady(obs []Observed, want int) bool {
	if len(obs) != want {
		return false
	}
	for _, o := range obs {
		if !o.Ready {
			return false
		}
	}
	return true
}

func reportObjects(obs []Observed) []Object {
	var out []Object
	for _, o := range obs {
		ob := o.Ref.object()
		ob.UID = o.UID
		ready := o.Ready
		ob.Ready = &ready
		out = append(out, ob)
	}
	return out
}

// settle resolves an uncertain apply by observing the cluster by annotation.
func (s *state) settle(ctx context.Context) error {
	obs, err := s.observeAll(ctx)
	if err != nil {
		// Without observation nothing may be claimed: leave the outcome unknown.
		return halt(ReasonObservation, "the cluster could not be observed: %v", err)
	}
	for _, o := range obs {
		if !s.owned(o) {
			s.report(ctx, Report{State: "failed", Reason: ReasonForeign, Objects: reportObjects(obs)})
			return halt(ReasonForeign, "%s/%s is not owned by this deployment", o.Ref.Kind, o.Ref.Name)
		}
	}
	switch {
	case len(obs) == 0:
		s.report(ctx, Report{State: "failed", Reason: ReasonApply})
		return halt(ReasonApply, "no object of the deployment exists; the apply did not take effect")
	case len(obs) < len(s.refs()):
		s.report(ctx, Report{State: "failed", Reason: ReasonPartial, Objects: reportObjects(obs)})
		return halt(ReasonPartial, "only %d of %d objects exist", len(obs), len(s.refs()))
	}
	return s.awaitAndSucceed(ctx)
}

// awaitAndSucceed waits for structured readiness, then reports it.
func (s *state) awaitAndSucceed(ctx context.Context) error {
	deadline := time.Now().Add(s.d.In.ReadinessTimeout)
	var obs []Observed
	for {
		var err error
		obs, err = s.observeAll(ctx)
		switch {
		case err != nil:
			// A transient observation failure is retried until the deadline.
		case len(obs) == len(s.refs()):
			for _, o := range obs {
				if !s.owned(o) {
					s.report(ctx, Report{State: "failed", Reason: ReasonForeign, Objects: reportObjects(obs)})
					return halt(ReasonForeign, "%s/%s is not owned by this deployment", o.Ref.Kind, o.Ref.Name)
				}
				if o.Failed {
					s.report(ctx, Report{State: "failed", Reason: ReasonMigration, Objects: reportObjects(obs)})
					return halt(ReasonMigration, "%s/%s failed", o.Ref.Kind, o.Ref.Name)
				}
			}
			if allPresentReady(obs, len(s.refs())) {
				goto ready
			}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			s.report(ctx, Report{State: "failed", Reason: ReasonRollout, Objects: reportObjects(obs)})
			return halt(ReasonRollout, "the objects were not all ready within %s", s.d.In.ReadinessTimeout)
		}
		select {
		case <-ctx.Done():
		case <-time.After(s.d.In.PollInterval):
		}
	}
ready:
	objs := reportObjects(obs)
	rec, err := s.d.Factory.Outcome(ctx, s.rec.ID, Report{State: "succeeded", Objects: objs})
	if err != nil {
		if errors.Is(err, ErrUnavailable) {
			// The live system is ready but the record is not settled; a retry attempt will observe and settle it.
			return halt(ReasonUnreported, "the success report could not be acknowledged")
		}
		return s.d.classify(err, "reporting readiness")
	}
	s.rec = rec.Deployment
	s.rec.Objects = objs
	return nil
}

func (s *state) evidence() any {
	return map[string]any{
		"deploymentId": s.rec.ID, "environmentId": s.rec.EnvironmentID, "configurationDigest": s.rec.ConfigurationDigest,
		"namespace": s.rec.Namespace, "source": s.rec.Source, "image": map[string]string{"artifactId": s.rec.Image.ArtifactID, "digest": s.rec.Image.Digest, "repository": s.plan.ImageRepo},
		"toolImage": s.cx.Command.Image, "objects": s.rec.Objects, "annotations": s.annots, "appUrl": appURL(s.snap), "expiresAt": s.rec.ExpiresAt, "kind": s.d.In.Kind,
	}
}
