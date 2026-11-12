//go:build component

package messaging_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/optimiware/kodama/internal/id"
	natsdriver "github.com/optimiware/kodama/internal/shell/app/drivers/nats"
	"github.com/optimiware/kodama/internal/shell/identity"
	broker "github.com/optimiware/kodama/internal/shell/integration/nats"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
	"github.com/optimiware/kodama/test/support/infrastructure/harness"
	support "github.com/optimiware/kodama/test/support/infrastructure/messaging"
)

// calloutValidity is the component test's current credential validity source.
// Expiry and revocation are applied by the directory's current-state resolve,
// so this only supplies a grant's upper bound.
type calloutValidity struct{}

func (calloutValidity) ValidUntil(context.Context, identity.Principal) (time.Time, error) {
	return time.Now().Add(time.Hour), nil
}

// startCalloutNATS starts the pinned NATS image with per-credential auth
// callout enabled. The generated account seed never leaves this process; the
// broker only receives the matching public account key.
func startCalloutNATS(t *testing.T) (*support.NATS, string, string, string) {
	t.Helper()
	// A deterministic test-only account pair. It is generated material for
	// this suite and is never a deployment value.
	const (
		issuerSeed = "SAACTERO2SL25CMPRBBN2BACIO6DSPU3BYBFOW34JHK2NU5Q342VTALZGE"
		issuerPub  = "AB33ITXG4OQ3KZZ6TGFBFLSYBNQNIWP5YC5LOTANFUZJNTZKCF7U2V7I"
	)

	const (
		shellUser     = "kodama-shell"
		shellPassword = "shell-password"
		calloutUser   = "kodama-auth"
		calloutPass   = "auth-password"
	)
	config := fmt.Sprintf(`port: 4222
server_name: kodama-callout
accounts {
  KODAMA: {
    users: [
      { user: %q, password: %q }
      { user: %q, password: %q }
    ]
  }
}
authorization {
  auth_callout {
    issuer: %q
    auth_users: [ %q, %q ]
    account: KODAMA
  }
}
`, shellUser, shellPassword, calloutUser, calloutPass, issuerPub, shellUser, calloutUser)

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("make config directory readable: %v", err)
	}
	path := filepath.Join(dir, "nats.conf")
	if err := os.WriteFile(path, []byte(config), 0o644); err != nil {
		t.Fatalf("write nats config: %v", err)
	}

	shard := harness.NewShard(t)
	ctx := shard.Context()
	digest := shard.RequireImage(ctx, harness.ImageNATS)
	container, err := shard.Run(ctx, harness.RunSpec{
		Image:   harness.ImageNATS,
		Ports:   []harness.Port{{Container: support.NATSClientPort}, {Container: support.NATSMonitorPort}},
		Volumes: []harness.Volume{{Name: dir, Target: "/etc/kodama-nats"}},
		Cmd:     []string{"-c", "/etc/kodama-nats/nats.conf", "-m", fmt.Sprintf("%d", support.NATSMonitorPort)},
	})
	if err != nil {
		t.Fatalf("start callout nats: %v", err)
	}
	clientAddr, err := container.Addr(ctx, support.NATSClientPort)
	if err != nil {
		t.Fatalf("nats client address: %v", err)
	}
	monitorAddr, err := container.Addr(ctx, support.NATSMonitorPort)
	if err != nil {
		t.Fatalf("nats monitor address: %v", err)
	}
	instance := &support.NATS{
		Shard:      shard,
		Container:  container,
		URL:        "nats://" + clientAddr,
		MonitorURL: "http://" + monitorAddr,
		Digest:     digest,
	}
	instance.WaitReady(t)
	support.RecordImage(harness.ImageNATS, digest, "nats-server "+instance.ServerVersion(t)+" (auth callout)")
	return instance, issuerSeed, shellUser + ":" + shellPassword, calloutUser + ":" + calloutPass
}

// splitUserPassword splits the test's "user:password" pair.
func splitUserPassword(t *testing.T, pair string) (string, string) {
	t.Helper()
	for index := 0; index < len(pair); index++ {
		if pair[index] == ':' {
			return pair[:index], pair[index+1:]
		}
	}
	t.Fatalf("credential pair %q has no separator", pair)
	return "", ""
}

// startCalloutService starts the authorization service over its own bypass
// connection and returns it. The caller keeps the instance context alive.
func startCalloutService(t *testing.T, instance *support.NATS, issuerSeed string, directory *support.Directory, calloutUser, calloutPassword string) *broker.Callout {
	t.Helper()
	ctx := instance.Shard.Context()
	client, err := natsdriver.Dial(ctx, natsdriver.Config{
		URL:            instance.URL,
		Name:           "per-credential-callout",
		User:           calloutUser,
		Password:       calloutPassword,
		ConnectTimeout: 5 * time.Second,
		ReconnectWait:  50 * time.Millisecond,
		MaxReconnects:  -1,
		DrainTimeout:   time.Second,
	})
	if err != nil {
		t.Fatalf("dial callout: %v", err)
	}
	callout, err := broker.NewCallout(broker.CalloutConfig{
		Connection: client,
		IssuerSeed: issuerSeed,
		Account:    "KODAMA",
		Principal:  directory,
		Authorizer: directory,
		Validity:   calloutValidity{},
		Clock:      support.RealClock{},
	})
	if err != nil {
		_ = client.Close(context.Background())
		t.Fatalf("build callout: %v", err)
	}
	if err := callout.Start(ctx); err != nil {
		_ = client.Close(context.Background())
		t.Fatalf("start callout: %v", err)
	}
	t.Cleanup(func() {
		_ = callout.Close(context.Background())
		_ = client.Close(context.Background())
	})
	return callout
}

// publishDecision connects one raw broker client presenting the credential and
// returns the server's publish-permission error, if any, for the subject.
func publishDecision(t *testing.T, instance *support.NATS, credential, subject, name string) error {
	t.Helper()
	connection, err := natsgo.Connect(instance.URL, natsgo.Token(credential), natsgo.Timeout(5*time.Second), natsgo.Name(name))
	if err != nil {
		t.Fatalf("dial %s: %v", name, err)
	}
	defer connection.Close()
	if err := connection.Publish(subject, []byte("{}")); err != nil {
		return err
	}
	if err := connection.Flush(); err != nil {
		return err
	}
	return connection.LastError()
}

// TestPerCredentialNATSAuthorizationRefusesUngrantedPublish pins the publish
// refusal on the real broker: a human credential and a service credential
// without event.publish are admitted to read their scope but the broker
// refuses every publish, including an out-of-scope event subject. An empty
// allow list with no deny would make the credential unrestricted.
func TestPerCredentialNATSAuthorizationRefusesUngrantedPublish(t *testing.T) {
	support.TrackTest(t)
	instance, issuerSeed, _, calloutPair := startCalloutNATS(t)
	calloutUser, calloutPassword := splitUserPassword(t, calloutPair)

	scope := liveScope("callout-publish")
	directory := support.NewDirectory()
	humanCredential := "credential-human-callout"
	directory.Add(humanCredential, identity.Principal{
		ID:      id.PrincipalID("human-callout"),
		Kind:    identity.KindHuman,
		Scope:   scope,
		Roles:   []tenancy.Role{tenancy.RoleEngineer},
		Enabled: true,
		Version: 1,
	})
	serviceCredential := "credential-service-callout-no-publish"
	serviceID := id.PrincipalID("service-callout-no-publish")
	directory.Add(serviceCredential, support.ServicePrincipal(serviceID, scope))
	directory.Grant(serviceID, "event.publish", false)
	publisherCredential := "credential-service-callout-publish"
	publisherID := id.PrincipalID("service-callout-publish")
	directory.Add(publisherCredential, support.ServicePrincipal(publisherID, scope))

	startCalloutService(t, instance, issuerSeed, directory, calloutUser, calloutPassword)

	// Positive control: the broker accepts an in-scope publish for a service
	// that holds event.publish, so a refused publish below is a permission
	// decision rather than a broken broker.
	ingress, err := broker.IngressSubject(scope)
	if err != nil {
		t.Fatalf("ingress subject: %v", err)
	}
	if err := publishDecision(t, instance, publisherCredential, string(ingress), "per-credential-granted-publish"); err != nil {
		t.Fatalf("granted service could not publish its ingress subject: %v", err)
	}
	support.Note(t, "granted_service_publish", "allowed")

	outOfScope, err := broker.EventSubject(liveScope("callout-publish-other"))
	if err != nil {
		t.Fatalf("out-of-scope event subject: %v", err)
	}
	for _, testCase := range []struct {
		name       string
		credential string
	}{
		{name: "human", credential: humanCredential},
		{name: "service without event.publish", credential: serviceCredential},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := publishDecision(t, instance, testCase.credential, string(outOfScope), "per-credential-denied-publish")
			if !errors.Is(err, natsgo.ErrPermissionViolation) {
				t.Fatalf("%s published out-of-scope subject %s: error = %v; want a publisher permission violation", testCase.name, outOfScope, err)
			}
		})
	}
	support.Note(t, "ungranted_publish_refused", "true")
}

// TestPerCredentialNATSAuthorizationCapsEstablishedConnectionLifetime pins
// the lifetime cap on the real broker: a raw external connection admitted with
// a current token does not survive the credential's full expiry. The grant
// lifetime is capped, so the broker closes the connection when the cap elapses
// and the client's reconnect re-runs the callout, which then refuses the
// revoked token.
func TestPerCredentialNATSAuthorizationCapsEstablishedConnectionLifetime(t *testing.T) {
	support.TrackTest(t)
	instance, issuerSeed, _, calloutPair := startCalloutNATS(t)
	calloutUser, calloutPassword := splitUserPassword(t, calloutPair)

	scope := liveScope("callout-cap")
	directory := support.NewDirectory()
	credential := "credential-service-callout-cap"
	directory.Add(credential, support.ServicePrincipal(id.PrincipalID("service-callout-cap"), scope))

	startCalloutService(t, instance, issuerSeed, directory, calloutUser, calloutPassword)

	// Reconnect a bounded number of times so a refused re-authentication ends
	// the connection instead of retrying forever.
	connection, err := natsgo.Connect(instance.URL, natsgo.Token(credential),
		natsgo.Timeout(5*time.Second),
		natsgo.Name("per-credential-cap"),
		natsgo.MaxReconnects(2),
		natsgo.ReconnectWait(500*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("current credential was refused by the broker callout: %v", err)
	}
	t.Cleanup(connection.Close)
	closed := make(chan struct{})
	var once sync.Once
	connection.SetClosedHandler(func(*natsgo.Conn) {
		once.Do(func() { close(closed) })
	})

	directory.Revoke(credential)
	started := time.Now()
	const boundedWait = 90 * time.Second
	select {
	case <-closed:
	case <-time.After(boundedWait):
		t.Fatalf("raw connection with a revoked credential stayed open for %s; want the broker to close it within %s",
			time.Since(started).Truncate(time.Millisecond), boundedWait)
	}
	t.Logf("raw connection closed %s after revocation", time.Since(started).Truncate(time.Millisecond))

	// A fresh connection with the same revoked token is refused, proving the
	// reconnect re-ran the callout rather than inheriting the old grant.
	if reconnected, err := natsgo.Connect(instance.URL, natsgo.Token(credential), natsgo.Timeout(5*time.Second), natsgo.Name("per-credential-cap-revoked")); err == nil {
		reconnected.Close()
		t.Fatal("revoked credential opened a new broker connection after its grant expired")
	}
	support.Note(t, "revoked_connection_closed_seconds", fmt.Sprintf("%.1f", time.Since(started).Seconds()))
}

// TestPerCredentialNATSAuthorization pins the real broker path: an established
// subscription for one credential closes on revocation or expiry through the
// live invalidation mechanism, and a new connection presenting the revoked or
// expired credential is refused by the authorization callout.
func TestPerCredentialNATSAuthorization(t *testing.T) {
	support.TrackTest(t)
	instance, issuerSeed, shellPair, calloutPair := startCalloutNATS(t)
	shellUser, shellPassword := splitUserPassword(t, shellPair)
	calloutUser, calloutPassword := splitUserPassword(t, calloutPair)
	ctx := instance.Shard.Context()

	dial := func(name, user, password string) *natsdriver.Client {
		t.Helper()
		client, err := natsdriver.Dial(ctx, natsdriver.Config{
			URL:            instance.URL,
			Name:           name,
			User:           user,
			Password:       password,
			ConnectTimeout: 5 * time.Second,
			ReconnectWait:  50 * time.Millisecond,
			MaxReconnects:  -1,
			DrainTimeout:   time.Second,
		})
		if err != nil {
			t.Fatalf("dial %s: %v", name, err)
		}
		return client
	}

	shellClient := dial("per-credential-shell", shellUser, shellPassword)
	t.Cleanup(func() { _ = shellClient.Close(context.Background()) })
	calloutClient := dial("per-credential-callout", calloutUser, calloutPassword)
	t.Cleanup(func() { _ = calloutClient.Close(context.Background()) })

	scope := liveScope("callout")
	expireScope := liveScope("callout-expire")
	directory := support.NewDirectory()
	credential := "credential-service-callout"
	directory.Add(credential, support.ServicePrincipal(id.PrincipalID("service-callout"), scope))
	expireCredential := "credential-service-callout-expire"
	directory.Add(expireCredential, support.ServicePrincipal(id.PrincipalID("service-callout-expire"), expireScope))

	callout, err := broker.NewCallout(broker.CalloutConfig{
		Connection: calloutClient,
		IssuerSeed: issuerSeed,
		Account:    "KODAMA",
		Principal:  directory,
		Authorizer: directory,
		Validity:   calloutValidity{},
		Clock:      support.RealClock{},
	})
	if err != nil {
		t.Fatalf("build callout: %v", err)
	}
	if err := callout.Start(ctx); err != nil {
		t.Fatalf("start callout: %v", err)
	}
	t.Cleanup(func() { _ = callout.Close(context.Background()) })

	adapter, err := broker.NewBroker(broker.BrokerConfig{
		Connect: func(context.Context) (broker.Client, error) {
			return dial("per-credential-established", shellUser, shellPassword), nil
		},
		Clock:               support.RealClock{},
		Principal:           directory,
		Authorizer:          directory,
		Sink:                support.NoopSink{},
		MaxReconnectBackoff: 40 * time.Millisecond,
		IdleValidation:      time.Hour,
		MaxPublishAttempts:  2,
	})
	if err != nil {
		t.Fatalf("build broker: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Close(context.Background()) })

	subject, err := broker.InvalidationSubject(scope)
	if err != nil {
		t.Fatalf("invalidation subject: %v", err)
	}

	// Positive control: the current credential is admitted by the real broker.
	valid, err := natsgo.Connect(instance.URL, natsgo.Token(credential), natsgo.Timeout(5*time.Second), natsgo.Name("per-credential-valid"))
	if err != nil {
		t.Fatalf("current credential was refused by the broker callout: %v", err)
	}
	valid.Close()

	subscription, err := adapter.Subscribe(ctx, broker.SubscriptionRequest{
		Credential: credential,
		Scope:      scope,
		Handler:    func(context.Context, token.Invalidation) error { return nil },
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = subscription.Close(context.Background()) })
	instance.WaitForSubscription(t, "per-credential-established", string(subject))

	// Revocation closes the already-established subscription and refuses a new
	// connection presenting the revoked credential.
	directory.Revoke(credential)
	payload, err := json.Marshal(token.Invalidation{
		Scope:       scope,
		PrincipalID: id.PrincipalID("service-callout"),
		ServiceID:   "service-1",
		Reason:      token.InvalidationRevoked,
		At:          testNow(),
	})
	if err != nil {
		t.Fatalf("encode invalidation: %v", err)
	}
	if err := shellClient.PublishCore(ctx, subject, payload); err != nil {
		t.Fatalf("publish revocation: %v", err)
	}
	instance.WaitForNoSubscription(t, "per-credential-established", string(subject))
	if connected, err := natsgo.Connect(instance.URL, natsgo.Token(credential), natsgo.Timeout(5*time.Second), natsgo.Name("per-credential-revoked")); err == nil {
		connected.Close()
		t.Fatal("revoked credential opened a new broker connection")
	}
	support.Note(t, "revoked_new_connection_refused", "true")

	// Expiry has the same result: the established subscription closes and a new
	// connection with the expired credential is refused.
	expireSubject, err := broker.InvalidationSubject(expireScope)
	if err != nil {
		t.Fatalf("expiry invalidation subject: %v", err)
	}
	expireSubscription, err := adapter.Subscribe(ctx, broker.SubscriptionRequest{
		Credential: expireCredential,
		Scope:      expireScope,
		Handler:    func(context.Context, token.Invalidation) error { return nil },
	})
	if err != nil {
		t.Fatalf("subscribe expiry credential: %v", err)
	}
	t.Cleanup(func() { _ = expireSubscription.Close(context.Background()) })
	instance.WaitForSubscription(t, "per-credential-established", string(expireSubject))

	directory.Expire(expireCredential)
	expirePayload, err := json.Marshal(token.Invalidation{
		Scope:       expireScope,
		PrincipalID: id.PrincipalID("service-callout-expire"),
		ServiceID:   "service-1",
		Reason:      token.InvalidationRevoked,
		At:          testNow(),
	})
	if err != nil {
		t.Fatalf("encode expiry invalidation: %v", err)
	}
	if err := shellClient.PublishCore(ctx, expireSubject, expirePayload); err != nil {
		t.Fatalf("publish expiry: %v", err)
	}
	instance.WaitForNoSubscription(t, "per-credential-established", string(expireSubject))
	if connected, err := natsgo.Connect(instance.URL, natsgo.Token(expireCredential), natsgo.Timeout(5*time.Second), natsgo.Name("per-credential-expired")); err == nil {
		connected.Close()
		t.Fatal("expired credential opened a new broker connection")
	}
	support.Note(t, "expired_new_connection_refused", "true")
}
