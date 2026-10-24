package app

// The PostgreSQL-backed construction of the model gateway and the MCP proxy
// (called by NewCore). It is the only place their real adapters are chosen; the
// slice itself is AssembleModelTools. Nothing here is exercised without a
// database (T-073/T-038 qualify the joined path); the pure parts it relies on
// are unit-tested in modeltools_test.go.

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/optimiware/kodama/internal/agents"
	agentsadapters "github.com/optimiware/kodama/internal/agents/adapters"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/gateway"
	gwadapters "github.com/optimiware/kodama/internal/gateway/adapters"
	"github.com/optimiware/kodama/internal/gateway/openai"
	gatewaypg "github.com/optimiware/kodama/internal/gateway/postgres"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/mcpproxy"
	mcpadapters "github.com/optimiware/kodama/internal/mcpproxy/adapters"
	mcppg "github.com/optimiware/kodama/internal/mcpproxy/postgres"
	mcpprotocol "github.com/optimiware/kodama/internal/mcpproxy/protocol"
	"github.com/optimiware/kodama/internal/runtimeauth"
	rtpg "github.com/optimiware/kodama/internal/runtimeauth/postgres"
	"github.com/optimiware/kodama/internal/secrets"
	secretspg "github.com/optimiware/kodama/internal/secrets/postgres"
	"github.com/optimiware/kodama/internal/storage"
)

// modelToolDeps are the already-built modules the gateway and MCP adapters
// bridge to.
type modelToolDeps struct {
	cfg            ModelToolsConfig
	env            Env
	installationID string
	log            *slog.Logger
	tuning         Tuning
	cursors        *httpapi.CursorCodec

	pool        *storage.Pool
	events      *eventspg.Store
	authority   identity.Authority
	access      identity.AccessReader
	secrets     *secretspg.Store
	keys        secrets.KeyWrapper
	runtime     *runtimeauth.Service
	attempts    rtpg.AttemptAuthority
	generation  func() int64
	executions  execution.Store
	tenants     execution.Tenants
	gatewayWait time.Duration
}

func certPool(path string) (*x509.CertPool, error) {
	if path == "" {
		return nil, nil
	}
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s holds no PEM certificate", path)
	}
	return pool, nil
}

// buildModelToolsPG builds the slice and the approvals the agent profile
// service consults. It requires the installation to have selected both a
// gateway origin and an MCP host; a half configuration is refused by
// ModelToolsConfig.Validate's pairing rule below, never silently dropped.
func buildModelToolsPG(d modelToolDeps) (*ModelToolSlice, agents.Approvals, error) {
	if len(d.cfg.Gateway.Origins) == 0 || len(d.cfg.MCP.AllowedHosts) == 0 {
		return nil, nil, errors.New("the model gateway and the MCP proxy are selected together: name an approved provider origin and an approved MCP host")
	}

	// ---- model gateway ----
	policy, err := d.cfg.gatewayPolicy(d.env)
	if err != nil {
		return nil, nil, err
	}
	estimator, err := d.cfg.estimator()
	if err != nil {
		return nil, nil, err
	}
	roots, err := certPool(d.cfg.Gateway.CAFile)
	if err != nil {
		return nil, nil, fmt.Errorf("gateway CA file: %w", err)
	}
	txAuth, err := gwadapters.NewTxAuthority(d.attempts, d.generation)
	if err != nil {
		return nil, nil, err
	}
	pins, err := gwadapters.NewPins(d.secrets)
	if err != nil {
		return nil, nil, err
	}
	gwStore, err := gatewaypg.NewStore(gatewaypg.Config{
		DB: d.pool, Events: gwadapters.EventsFrom(d.events), Workload: txAuth, Management: gwadapters.BindManagement, Pins: pins,
	})
	if err != nil {
		return nil, nil, err
	}
	gwAuthority, err := gwadapters.NewAuthority(d.authority)
	if err != nil {
		return nil, nil, err
	}
	workloads, err := gwadapters.NewRuntimeWorkloads(d.runtime)
	if err != nil {
		return nil, nil, err
	}
	client, err := openai.NewClient(openai.Config{
		Policy: policy, TLS: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, Stream: openai.DefaultStreamLimits(),
		Timeouts: openai.DefaultTimeouts(),
	})
	if err != nil {
		return nil, nil, err
	}
	versions, err := secretspg.NewVersionSource(d.secrets, d.keys, d.installationID)
	if err != nil {
		return nil, nil, err
	}
	gwCreds, err := gwadapters.NewCredentials(versions, d.secrets)
	if err != nil {
		return nil, nil, err
	}
	gwSvc, err := gateway.NewService(gateway.Config{
		Store: gwStore, Workloads: workloads, Authority: gwAuthority, Credentials: gwCreds, Provider: client,
		Estimator: estimator, Decoder: openai.NewDecoder(), Destinations: policy, Tenants: d.tenants, Logger: d.log,
	})
	if err != nil {
		return nil, nil, err
	}

	// ---- MCP proxy ----
	mcpAuth, err := mcpadapters.NewRuntimeAuthority(d.runtime, d.attempts, d.generation)
	if err != nil {
		return nil, nil, err
	}
	mcpPins, err := mcpadapters.Pins(d.secrets)
	if err != nil {
		return nil, nil, err
	}
	mcpStore, err := mcppg.NewStore(d.pool, mcppg.Options{Recheck: mcpAuth.Recheck, Pin: mcpPins})
	if err != nil {
		return nil, nil, err
	}
	human, err := mcpadapters.NewHumanAuthority(d.authority, d.access)
	if err != nil {
		return nil, nil, err
	}
	mcpCreds, err := mcpadapters.NewCredentials(versions)
	if err != nil {
		return nil, nil, err
	}
	dest, err := mcpprotocol.NewDestinations(mcpprotocol.DestinationConfig{Allowed: d.cfg.MCP.AllowedHosts, AllowPrivate: d.cfg.MCP.AllowPrivate})
	if err != nil {
		return nil, nil, err
	}
	mcpRoots, err := certPool(d.cfg.MCP.CAFile)
	if err != nil {
		return nil, nil, fmt.Errorf("MCP CA file: %w", err)
	}
	remote, err := mcpprotocol.NewRemote(mcpprotocol.RemoteConfig{Destinations: dest, RootCAs: mcpRoots, ConnectTimeout: 10 * time.Second, MaxResultBytes: 1 << 20})
	if err != nil {
		return nil, nil, err
	}
	steps, err := mcpadapters.NewExecutionSteps(d.executions)
	if err != nil {
		return nil, nil, err
	}
	attemptSource, err := agentsadapters.NewAttemptSource(d.executions)
	if err != nil {
		return nil, nil, err
	}
	policyReader, err := agentsadapters.NewPolicyReader(attemptSource)
	if err != nil {
		return nil, nil, err
	}
	mcpPolicy, err := mcpadapters.NewPolicy(steps, policyReader)
	if err != nil {
		return nil, nil, err
	}
	incarnation, err := newIncarnation()
	if err != nil {
		return nil, nil, err
	}
	mcpSvc, err := mcpproxy.NewService(mcpproxy.Config{
		Store: mcpStore, Human: human, Runtime: mcpAuth, Policy: mcpPolicy, Credentials: mcpCreds, Remote: remote,
		Destinations: dest, Secrets: mcpStore, Logger: d.log,
		Options: mcpproxy.Options{
			Protocol: mcpprotocol.ProtocolRevision, Incarnation: incarnation, SessionTTL: time.Hour, CallTimeout: 30 * time.Second,
			RecheckInterval: time.Second, StaleAfter: 2 * time.Minute, MaxArgumentBytes: 64 << 10, MaxResultBytes: 1 << 20, MaxDiscoveryPages: 10,
		},
	})
	if err != nil {
		return nil, nil, err
	}

	// ---- approvals: the models the stored settings enable, the tools the
	// registrations enable ----
	modelApprovals, err := NewModelApprovals(gwStore)
	if err != nil {
		return nil, nil, err
	}
	toolApprovals, err := mcpadapters.NewToolApprovals(mcpStore)
	if err != nil {
		return nil, nil, err
	}
	approvals := ComposedApprovals{Models: modelApprovals, Tools: toolApprovals}

	slice, err := AssembleModelTools(ModelToolInputs{
		Env: d.env, Log: d.log, Tuning: d.tuning, Cursors: d.cursors, Tenants: d.tenants,
		Gateway: gwSvc, GatewayWriteTimeout: d.gatewayWait, MCP: mcpSvc,
		Adapters: []any{gwStore, gwAuthority, workloads, client, gwCreds, mcpStore, mcpAuth, human, mcpCreds, remote, mcpPolicy},
	})
	if err != nil {
		return nil, nil, err
	}
	return slice, approvals, nil
}

// newIncarnation names this process: live MCP handles are process-owned, so a
// session recorded by another incarnation is rejected, never adopted.
func newIncarnation() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "inc-" + hex.EncodeToString(b[:]), nil
}
