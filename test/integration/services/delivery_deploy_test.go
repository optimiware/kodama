//go:build integration

package services

// Delivery recipe: protected promotion of the tested digest through a
// deploy-repository commit, and a real deploy mutation whose response was lost
// is observed before any repetition.
//
// The promotion scenario publishes one disposable image to the real registry,
// runs those exact bytes as the `test` action, writes a deploy-repository
// candidate commit that records the tested digest-pinned reference, requests a
// reviewer approval for the exact ref/base/candidate and only then moves the
// protected `refs/heads/deploy` ref. The promotion execution has no build
// action, the registry tag does not move, and the same digest is still served
// afterwards: promotion reuses the tested digest instead of rebuilding it.
//
// The lost-response scenario sends the real protected ref update through a
// labelled HTTPS relay that discards the server's answer after the Git service
// accepted and reported it. The authored integrate action requires that attempt
// to fail, reads the protected ref back through the ordinary workload origin,
// and exits successfully only when the observation returns the exact candidate.
// It performs no authored repetition, so the deploy history has exactly one new
// commit and the durable journal has exactly one reported update for that
// candidate. Git's own transport retry of the identical exchange, if any, still
// carries the same candidate and changes neither fact.

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/userland/git"
	"github.com/optimiware/kodama/test/support/factory"
	support "github.com/optimiware/kodama/test/support/infrastructure/messaging"
)

const (
	// deliveryDeployRef is the approval-guarded deploy ref.
	deliveryDeployRef = "refs/heads/deploy"
	// deliveryDeployWorkRef is the granted work branch the release action
	// pushes its candidate to.
	deliveryDeployWorkRef = "refs/heads/work/deploy-candidate"
	// deliveryDeployTimeout bounds one durable Git fact the served path must
	// produce.
	deliveryDeployTimeout = 2 * time.Minute
)

// TestDeliveryProtectedDeployPromotion proves the tested digest is promoted
// through a real deploy-repository commit under a real reviewer approval, with
// no build action and no registry republish.
func TestDeliveryProtectedDeployPromotion(t *testing.T) {
	harness, gitFixture := deliveryStartDeployHarness(t)
	image := gitFixture.EnsureWorkloadImage(t)
	registry := support.StartRegistry(t, false)
	published := deliveryPublishImage(t, registry, harness.Image, "kodama/delivery-deploy", "build-1")
	base := deliveryDeployBase(t, gitFixture)

	// Run the exact published bytes first. This execution proves the digest is
	// the tested one; the promotion execution below receives it as an admitted
	// input and has no build action.
	testApplied := deliveryApply(t, harness, "delivery-deploy-test", deliveryPipelineDoc{
		Actions: []deliveryActionDoc{{
			Name:    "test",
			Image:   published.Pinned,
			Timeout: deliveryActionTimeout,
			Commands: []string{
				`test -f /etc/alpine-release`,
				`test "$(cat /kodama-delivery-marker)" = '` + published.Marker + `'`,
			},
		}},
	})
	testStarted := deliveryStart(t, harness, testApplied.PipelineID, nil, "start-delivery-deploy-test")
	deliveryRequireSucceeded(t, harness, testStarted.ExecutionID)

	release, err := gitFixture.WorkloadDeployReleaseCommands(t, factory.DeployCandidateSpec{
		WorkRef:        deliveryDeployWorkRef,
		ExpectedBase:   base,
		ImageReference: published.Pinned,
	})
	if err != nil {
		t.Fatalf("author the deploy release commands: %v", err)
	}
	integrate, err := gitFixture.WorkloadDeployIntegrateCommands(t, factory.DeployCandidateSpec{
		WorkRef:        deliveryDeployWorkRef,
		ExpectedBase:   base,
		ImageReference: published.Pinned,
	})
	if err != nil {
		t.Fatalf("author the deploy integrate commands: %v", err)
	}

	applied := deliveryApply(t, harness, "delivery-deploy-promote", deliveryPipelineDoc{
		Inputs: []string{"image"},
		Actions: []deliveryActionDoc{
			{
				Name:     "release",
				Image:    image.RunnableReference,
				Timeout:  deliveryActionTimeout,
				Outputs:  []string{"candidate", "expected_base", "image"},
				Commands: release,
			},
			{
				Name:    "approve",
				Request: "role/reviewer",
				Timeout: deliveryActionTimeout,
				Subject: map[string]any{
					"operation":     "git.ref.update",
					"resource":      gitFixture.DeployResourceID,
					"ref":           deliveryDeployRef,
					"candidate":     map[string]string{"from": "release.outputs.candidate"},
					"expected_base": map[string]string{"from": "release.outputs.expected_base"},
					"image":         map[string]string{"from": "release.outputs.image"},
				},
				Outputs: []string{"approved"},
			},
			{
				Name:     "integrate",
				Image:    image.RunnableReference,
				Timeout:  deliveryActionTimeout,
				Outputs:  []string{"deployed"},
				Commands: integrate,
			},
		},
	})
	started := deliveryStart(t, harness, applied.PipelineID,
		map[string]string{"image": published.Pinned}, "start-delivery-deploy-promote")
	waiting := deliveryWaitActionRunning(t, harness, started.ExecutionID, "approve")

	// While the request is pending the protected deploy ref still points at the
	// reviewed base and the integrate action has not started.
	if ref := gitFixture.RefCommit(t, factory.GitDeployRepository, deliveryDeployRef); ref != base {
		t.Fatalf("protected deploy ref = %q while the request is pending; want the base %q", ref, base)
	}
	pending := inspectExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	if state := actionByName(t, pending, "integrate").State; state == "running" || state == "succeeded" {
		t.Fatalf("integrate action state = %q while the request is pending; want it unstarted", state)
	}
	subject, err := harness.RequestSubject(t.Context(), started.ExecutionID, "approve")
	if err != nil {
		t.Fatalf("read the persisted deploy approval subject: %v", err)
	}
	for key, want := range map[string]string{
		"operation": "git.ref.update",
		"resource":  gitFixture.DeployResourceID,
		"ref":       deliveryDeployRef,
		"image":     published.Pinned,
	} {
		value, ok := subject[key]
		if !ok {
			t.Fatalf("deploy approval subject has no %q field: %+v", key, subject)
		}
		if value.Literal != want {
			t.Fatalf("deploy approval subject %q = %q; want %q", key, value.Literal, want)
		}
	}

	deliveryResolveRequest(t, harness, harness.ReviewerToken, waiting, "approve", "pass",
		map[string]string{"approved": "approved"}, "resolve-delivery-deploy")

	execution := deliveryRequireSucceeded(t, harness, started.ExecutionID)
	names := make([]string, 0, len(execution.Actions))
	for _, action := range execution.Actions {
		names = append(names, action.Name)
		if action.Name == "build" {
			t.Fatalf("promotion execution ran a build action; it must only promote the tested digest (actions=%v)", names)
		}
	}
	if len(names) != 3 || names[0] != "release" || names[1] != "approve" || names[2] != "integrate" {
		t.Fatalf("promotion execution actions = %v; want [release approve integrate]", names)
	}
	deployed := actionByName(t, execution, "integrate").Outputs["deployed"]
	candidate := actionByName(t, execution, "release").Outputs["candidate"]
	if candidate == "" || candidate == base {
		t.Fatalf("release candidate = %q; want a commit different from the base %q", candidate, base)
	}
	if deployed != candidate {
		t.Fatalf("deployed output = %q; want the exact protected candidate %q", deployed, candidate)
	}

	// The deploy repository commit is real: the protected ref is the candidate,
	// its only parent is the reviewed base, and the candidate carries the exact
	// tested digest-pinned reference.
	if ref := gitFixture.RefCommit(t, factory.GitDeployRepository, deliveryDeployRef); ref != candidate {
		t.Fatalf("protected deploy ref = %q; want the promoted candidate %q", ref, candidate)
	}
	if parents := gitFixture.CommitParents(t, factory.GitDeployRepository, candidate); len(parents) != 1 || parents[0] != base {
		t.Fatalf("deploy candidate parents = %v; want exactly the reviewed base %q", parents, base)
	}
	if content := strings.TrimSpace(gitFixture.FileAt(t, factory.GitDeployRepository, candidate, "deploy/image")); content != published.Pinned {
		t.Fatalf("deploy candidate records image %q; want the tested digest %q", content, published.Pinned)
	}
	if ref := gitFixture.RefCommit(t, factory.GitDeployRepository, deliveryDeployWorkRef); ref != candidate {
		t.Fatalf("deploy work ref = %q; want the candidate %q", ref, candidate)
	}

	// The release action proved protection: the same execution's unapproved
	// push to the guarded ref was refused, and the later approved push moved it.
	logs := deliveryActionLogs(t, harness, started.ExecutionID, actionByName(t, execution, "release").ActionID)
	if !strings.Contains(logs, "unapproved-deploy-push-refused") {
		t.Fatalf("release logs do not record the unapproved protected push refusal:\n%s", logs)
	}

	// The digest is still exactly served and the tag did not move: promotion
	// neither rebuilt nor republished the image.
	deliveryRequireRegistryDigest(t, registry, published)
	tagDigest, found := deliveryManifestDigest(registry.BaseURL, published.Repository, published.Tag)
	if !found || tagDigest != published.Digest {
		t.Fatalf("registry tag %s:%s = %q (found=%t); want the tested digest %q unchanged",
			published.Repository, published.Tag, tagDigest, found, published.Digest)
	}

	entry := deliveryWaitDeployJournal(t, gitFixture, candidate)
	if entry.ExpectedBase != base || entry.Candidate != candidate || entry.State != git.JournalReported {
		t.Fatalf("deploy journal = %+v; want a reported %s -> %s update", entry, base, candidate)
	}
	row := deliveryDeployVerification(t, gitFixture, started.ExecutionID, candidate)
	if row.ID != entry.VerificationID || row.Operation != "git.ref.update" || row.Result != "approved" || !row.Approved {
		t.Fatalf("deploy verification = %+v; want the journal's approved git.ref.update", row)
	}
	if row.BindingID != gitFixture.DeployBindingID || row.ResourceID != gitFixture.DeployResourceID {
		t.Fatalf("deploy verification identity = %+v; want the deploy binding", row)
	}
	approval, err := gitFixture.RequestedApproval(t.Context(), row.ID)
	if err != nil {
		t.Fatalf("read the stored deploy approval: %v", err)
	}
	if got := approval.String(); got != "role/reviewer" {
		t.Fatalf("stored deploy approval = %q; want role/reviewer", got)
	}
	event, ok, err := gitFixture.ReportEvent(t.Context(), row.ID)
	if err != nil {
		t.Fatalf("read the linked deploy report event: %v", err)
	}
	if !ok || event.Type != "resource.verified" || event.ReportType != "git.ref.updated" ||
		event.SubjectID != gitFixture.DeployResourceID || event.CausationID != row.ID {
		t.Fatalf("linked deploy report event = %+v present=%t; want resource.verified/git.ref.updated for the deploy resource", event, ok)
	}
	deliveryAssertNoArtifactEntity(t)
}

// TestDeliveryDeployLostResponseConverges proves a real protected deploy
// mutation whose response was lost is observed through the repository before
// any repetition: the accepted candidate remains the single new deploy commit
// and the observation converges without a second update.
func TestDeliveryDeployLostResponseConverges(t *testing.T) {
	harness, gitFixture := deliveryStartDeployHarness(t)
	image := gitFixture.EnsureWorkloadImage(t)
	registry := support.StartRegistry(t, false)
	published := deliveryPublishManifest(t, registry, "kodama/delivery-deploy-lost", "deploy-1")
	base := deliveryDeployBase(t, gitFixture)

	lostBase, receivePacks := deliveryLostDeployRelay(t, harness.TLS, gitFixture.HostURL)
	lostURL := lostBase + "/" + factory.GitDeployRepository + ".git"
	release, err := gitFixture.WorkloadDeployReleaseCommands(t, factory.DeployCandidateSpec{
		WorkRef:        deliveryDeployWorkRef,
		ExpectedBase:   base,
		ImageReference: published.Pinned,
	})
	if err != nil {
		t.Fatalf("author the lost deploy release commands: %v", err)
	}
	integrate, err := gitFixture.WorkloadDeployIntegrateCommands(t, factory.DeployCandidateSpec{
		WorkRef:        deliveryDeployWorkRef,
		ExpectedBase:   base,
		ImageReference: published.Pinned,
		LostPushURL:    lostURL,
		ObserveURL:     gitFixture.WorkloadRepositoryURL(factory.GitDeployRepository),
	})
	if err != nil {
		t.Fatalf("author the lost deploy integrate commands: %v", err)
	}

	applied := deliveryApply(t, harness, "delivery-deploy-lost", deliveryPipelineDoc{
		Inputs: []string{"image"},
		Actions: []deliveryActionDoc{
			{
				Name:     "release",
				Image:    image.RunnableReference,
				Timeout:  deliveryActionTimeout,
				Outputs:  []string{"candidate", "expected_base", "image"},
				Commands: release,
			},
			{
				Name:    "approve",
				Request: "role/reviewer",
				Timeout: deliveryActionTimeout,
				Subject: map[string]any{
					"operation":     "git.ref.update",
					"resource":      gitFixture.DeployResourceID,
					"ref":           deliveryDeployRef,
					"candidate":     map[string]string{"from": "release.outputs.candidate"},
					"expected_base": map[string]string{"from": "release.outputs.expected_base"},
					"image":         map[string]string{"from": "release.outputs.image"},
				},
				Outputs: []string{"approved"},
			},
			{
				Name:     "integrate",
				Image:    image.RunnableReference,
				Timeout:  deliveryActionTimeout,
				Outputs:  []string{"deployed"},
				Commands: integrate,
			},
		},
	})
	started := deliveryStart(t, harness, applied.PipelineID,
		map[string]string{"image": published.Pinned}, "start-delivery-deploy-lost")
	waiting := deliveryWaitActionRunning(t, harness, started.ExecutionID, "approve")
	deliveryResolveRequest(t, harness, harness.ReviewerToken, waiting, "approve", "pass",
		map[string]string{"approved": "approved"}, "resolve-delivery-deploy-lost")

	execution := deliveryRequireSucceeded(t, harness, started.ExecutionID)
	candidate := actionByName(t, execution, "release").Outputs["candidate"]
	if candidate == "" || candidate == base {
		t.Fatalf("release candidate = %q; want a commit different from the base %q", candidate, base)
	}
	if got := actionByName(t, execution, "integrate").Outputs["deployed"]; got != candidate {
		t.Fatalf("deployed output = %q; want the observed candidate %q", got, candidate)
	}

	// The mutation went through the lossy relay. Git may retry the identical
	// HTTP exchange after the dropped answer; that transport retry carries the
	// same candidate and cannot create a second commit. The action itself
	// performs no authored repetition: it observes the accepted ref and exits.
	if got := receivePacks(); got < 1 {
		t.Fatalf("lossy relay observed no receive-pack request; the loss scenario was not exercised")
	} else {
		t.Logf("lossy relay observed %d receive-pack request(s) for the one candidate", got)
	}
	logs := deliveryActionLogs(t, harness, started.ExecutionID, actionByName(t, execution, "integrate").ActionID)
	if !strings.Contains(logs, "observed-accepted-deploy-before-repetition") {
		t.Fatalf("integrate logs do not record the observation before repetition:\n%s", logs)
	}
	ref := gitFixture.RefCommit(t, factory.GitDeployRepository, deliveryDeployRef)
	if ref != candidate {
		t.Fatalf("protected deploy ref = %q; want the observed candidate %q", ref, candidate)
	}
	history := gitFixture.HistoryAt(t, factory.GitDeployRepository, deliveryDeployRef)
	if len(history) != 2 || history[0] != candidate || history[1] != base {
		t.Fatalf("deploy history = %v; want exactly [%s %s] with no duplicate commit", history, candidate, base)
	}
	if content := strings.TrimSpace(gitFixture.FileAt(t, factory.GitDeployRepository, candidate, "deploy/image")); content != published.Pinned {
		t.Fatalf("deploy candidate records image %q; want %q", content, published.Pinned)
	}

	// Exactly one reported update exists for the observed candidate.
	reported := 0
	for _, entry := range gitFixture.JournalEntries(t) {
		if entry.Repository == factory.GitDeployRepository && entry.Ref == deliveryDeployRef &&
			entry.Candidate == candidate && entry.State == git.JournalReported {
			reported++
		}
	}
	if reported != 1 {
		t.Fatalf("reported deploy journal entries for %s = %d; want exactly one", candidate, reported)
	}
	row := deliveryDeployVerification(t, gitFixture, started.ExecutionID, candidate)
	if row.Result != "approved" || !row.Approved || row.Operation != "git.ref.update" {
		t.Fatalf("deploy verification = %+v; want one approved git.ref.update", row)
	}
	deliveryRequireRegistryDigest(t, registry, published)
	deliveryAssertNoArtifactEntity(t)
}

// deliveryStartDeployHarness starts the real factory with the host Git service
// and requires the deploy repository binding these scenarios exercise.
func deliveryStartDeployHarness(t *testing.T) (*factory.Harness, *factory.GitFixture) {
	t.Helper()
	harness := factory.Start(t, factory.WithGitService())
	if harness.Git == nil {
		t.Fatal("factory.WithGitService did not expose a Git fixture")
	}
	if harness.Git.DeployRepository != factory.GitDeployRepository || harness.Git.DeployResourceID == "" {
		t.Fatalf("Git fixture deploy repository = %q/%q; want %q with a registered resource",
			harness.Git.DeployRepository, harness.Git.DeployResourceID, factory.GitDeployRepository)
	}
	return harness, harness.Git
}

// deliveryDeployBase returns the seeded deploy ref the promotion must build on.
func deliveryDeployBase(t *testing.T, gitFixture *factory.GitFixture) string {
	t.Helper()
	base := gitFixture.RefCommit(t, factory.GitDeployRepository, deliveryDeployRef)
	if base == "" {
		t.Fatalf("the seeded %s ref is missing from %s", deliveryDeployRef, factory.GitDeployRepository)
	}
	return base
}

// deliveryWaitDeployJournal waits for the reported deploy update of one
// candidate.
func deliveryWaitDeployJournal(t *testing.T, gitFixture *factory.GitFixture, candidate string) git.JournalEntry {
	t.Helper()
	var found git.JournalEntry
	gitFixture.WaitFor(t, fmt.Sprintf("reported deploy journal candidate=%s", candidate),
		deliveryDeployTimeout, func() bool {
			for _, entry := range gitFixture.JournalEntries(t) {
				if entry.Repository == factory.GitDeployRepository && entry.Ref == deliveryDeployRef &&
					entry.Candidate == candidate && entry.State == git.JournalReported {
					found = entry
					return true
				}
			}
			return false
		})
	return found
}

// deliveryDeployVerification returns the approved verification of one deploy
// candidate in one execution.
func deliveryDeployVerification(t *testing.T, gitFixture *factory.GitFixture, executionID, candidate string) factory.VerificationRow {
	t.Helper()
	var found factory.VerificationRow
	gitFixture.WaitFor(t, fmt.Sprintf("deploy verification execution=%s candidate=%s", executionID, candidate),
		deliveryDeployTimeout, func() bool {
			rows, err := gitFixture.Verifications(context.Background())
			if err != nil {
				return false
			}
			for _, row := range rows {
				if row.ExecutionID != executionID || row.ResourceID != gitFixture.DeployResourceID ||
					row.Operation != "git.ref.update" || row.Result != "approved" || !row.Approved ||
					!strings.Contains(row.Subject, deliveryDeployRef) {
					continue
				}
				if strings.Contains(row.Subject, candidate) {
					found = row
					return true
				}
			}
			return false
		})
	return found
}

// deliveryActionLogs reads the retained stdout and stderr of one action.
func deliveryActionLogs(t *testing.T, harness *factory.Harness, executionID, actionID string) string {
	t.Helper()
	var logs strings.Builder
	for _, stream := range []string{"stdout", "stderr"} {
		result, publicError := harness.REST(harness.ProjectToken, "execution.logs.read", api.ExecutionLogsReadRequest{
			ExecutionID: executionID,
			ActionID:    actionID,
			Stream:      stream,
		}, nil, "")
		if publicError.Code != "" {
			t.Fatalf("read %s logs of action %s: %s (%s)", stream, actionID, publicError.Code, publicError.Message)
		}
		var read api.ExecutionLogsReadResult
		harness.Decode(result, &read)
		for _, chunk := range read.Chunks {
			_, _ = logs.Write(chunk.Data)
		}
	}
	return logs.String()
}

// deliveryLostDeployRelay serves the fixture TLS certificate on the workload
// host and reverse-proxies smart-HTTP requests to the host Git origin. Every
// exchange except a POST git-receive-pack is forwarded unchanged. For the
// mutation it reads the upstream answer to completion first (the Git service
// has accepted and reported the ref update by then), then closes the client
// connection without forwarding any answer: the client observes a lost
// response while the durable effect remains.
//
// The returned receive-pack counter proves the scenario exercised the lossy
// path. Git may retry the identical transport exchange; the counter reports
// that, while the durable journal and history assertions prove the mutation
// itself stayed single.
func deliveryLostDeployRelay(t *testing.T, tlsFixture *factory.TLSFixture, targetURL string) (string, func() int) {
	t.Helper()
	if tlsFixture == nil {
		t.Fatal("the lossy deploy relay needs the fixture TLS material")
	}
	target, err := url.Parse(targetURL)
	if err != nil || target.Host == "" || target.Scheme != "https" {
		t.Fatalf("parse Git origin %q: %v", targetURL, err)
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(deliveryWorkloadHost(t), "0"))
	if err != nil {
		t.Fatalf("listen for the lossy deploy relay: %v", err)
	}
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok || address.Port <= 0 {
		_ = listener.Close()
		t.Fatalf("lossy deploy relay address = %v; want a TCP port", listener.Addr())
	}

	var (
		mu           sync.Mutex
		receivePacks int
	)
	client := tlsFixture.Client()
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upstream := target.Scheme + "://" + target.Host + request.URL.RequestURI()
		forward, err := http.NewRequestWithContext(request.Context(), request.Method, upstream, request.Body)
		if err != nil {
			http.Error(writer, "lossy relay: build upstream request", http.StatusBadGateway)
			return
		}
		forward.ContentLength = request.ContentLength
		forward.Header = request.Header.Clone()
		removeHopByHop(forward.Header)
		response, err := client.Do(forward)
		if err != nil {
			http.Error(writer, "lossy relay: upstream request", http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		if request.Method == http.MethodPost && strings.Contains(request.URL.Path, "git-receive-pack") {
			// Drain the real answer first. The smart-HTTP backend buffers its
			// whole response, so a complete read means the ref update and its
			// report are durable. Then close the client connection with no
			// answer at all.
			if _, err := io.Copy(io.Discard, response.Body); err != nil {
				http.Error(writer, "lossy relay: drain upstream response", http.StatusBadGateway)
				return
			}
			mu.Lock()
			receivePacks++
			mu.Unlock()
			hijacker, ok := writer.(http.Hijacker)
			if !ok {
				http.Error(writer, "lossy relay: hijack unsupported", http.StatusInternalServerError)
				return
			}
			connection, _, err := hijacker.Hijack()
			if err == nil {
				_ = connection.Close()
			}
			return
		}
		removeHopByHop(response.Header)
		for key, values := range response.Header {
			writer.Header()[key] = append([]string(nil), values...)
		}
		writer.WriteHeader(response.StatusCode)
		_, _ = io.Copy(writer, response.Body)
	})

	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
	}
	served := make(chan error, 1)
	go func() {
		served <- server.Serve(tls.NewListener(listener, tlsFixture.ServerConfig()))
	}()
	t.Cleanup(func() {
		_ = server.Close()
		_ = listener.Close()
		select {
		case <-served:
		case <-time.After(10 * time.Second):
			t.Errorf("lossy deploy relay did not stop within 10s")
		}
	})

	return fmt.Sprintf("https://host.containers.internal:%d", address.Port), func() int {
		mu.Lock()
		defer mu.Unlock()
		return receivePacks
	}
}

// removeHopByHop drops headers that belong to one connection, never to the
// proxied message.
func removeHopByHop(header http.Header) {
	for _, name := range []string{
		"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
		"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
	} {
		header.Del(name)
	}
}
