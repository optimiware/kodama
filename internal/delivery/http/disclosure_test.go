package deliveryhttp_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/delivery"
	deliveryhttp "github.com/optimiware/kodama/internal/delivery/http"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
)

// memStore is the in-memory environment half of delivery.Store. Anything else
// is unimplemented on purpose: the disclosure flows touch environments only.
type memStore struct {
	delivery.Store
	mu   sync.Mutex
	envs map[domain.EnvironmentID]delivery.Environment
}

func (m *memStore) CreateEnvironment(_ context.Context, _ delivery.Mutation, env delivery.Environment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.envs[env.ID()] = env
	return nil
}

func (m *memStore) GetEnvironment(_ context.Context, _ domain.Scope, id domain.EnvironmentID) (delivery.Environment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	env, ok := m.envs[id]
	if !ok {
		return delivery.Environment{}, delivery.Fail(delivery.ErrNotFound, nil, "not found")
	}
	return env, nil
}

func (m *memStore) ListEnvironments(context.Context, domain.Scope, string, int) ([]delivery.Environment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]delivery.Environment, 0, len(m.envs))
	for _, e := range m.envs {
		out = append(out, e)
	}
	return out, nil
}

func (m *memStore) UpdateEnvironment(_ context.Context, mu delivery.Mutation, _ domain.Scope, id domain.EnvironmentID, expected domain.Version, edit delivery.EnvironmentEdit) (delivery.Environment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	env, ok := m.envs[id]
	if !ok {
		return delivery.Environment{}, delivery.Fail(delivery.ErrNotFound, nil, "not found")
	}
	if _, err := env.Edit(expected, edit, mu.Now); err != nil {
		return delivery.Environment{}, err
	}
	m.envs[id] = env
	return env, nil
}

// credentials is the secrets-owner port: it answers metadata for known
// references and fails with the credential's value inside the *cause* of the
// error, which is how a careless dependency leaks.
type credentials struct{}

func (credentials) Current(_ context.Context, _ domain.Scope, ref string) (delivery.CredentialInfo, error) {
	switch ref {
	case "secret://project/kubeconfig":
		return delivery.CredentialInfo{Ref: ref, Kind: delivery.KindKubeconfig, Version: 1}, nil
	case "secret://project/broken":
		return delivery.CredentialInfo{}, errors.New("decrypt failed for value " + sentinel)
	case "secret://project/leaky":
		return delivery.CredentialInfo{}, delivery.Fail(delivery.ErrCredentialUnavailable, errors.New("value="+sentinel), "a credential is not available in this project")
	}
	return delivery.CredentialInfo{}, delivery.Fail(delivery.ErrCredentialUnavailable, nil, "a credential is not available in this project")
}

type allowAll struct{}

func (allowAll) Authorize(context.Context, delivery.Actor, domain.Scope, delivery.Permission) error {
	return nil
}

// Test_delivery_disclosure proves a credential value never appears in a response,
// a stored (replayable) response body, an error or a log line, even when a
// dependency's own error text carries it: the module holds references and pinned
// versions only, renders fixed messages, and logs operation names and safe
// identifiers. No external service is involved.
func Test_delivery_disclosure(t *testing.T) {
	sp := loadSpec(t)
	logs := &logBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	svc, err := delivery.NewService(delivery.Config{
		Store: &memStore{envs: map[domain.EnvironmentID]delivery.Environment{}}, Access: allowAll{}, Credentials: credentials{}, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	a := newAPI(t)
	// Rebuild the public router over the real service so its logs are captured too.
	router, err := httpapi.NewRouter(httpapi.RouterConfig{Authenticator: tokens{"tok-engineer": "u_engineer"}, Authorizer: a.authz, RateLimiter: httpapi.Unlimited(),
		Prefix: "/api/v1", Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	h, err := deliveryhttp.NewHandler(svc, a.reg, a.codec, deliveryhttp.DefaultActions())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.RegisterRoutes(router); err != nil {
		t.Fatal(err)
	}
	send := func(method, path, body string, hdr ...string) resp {
		return a.send(router, "tok-engineer", method, "/api/v1"+pubBase+path, body, hdr...)
	}

	env := func(name, ref string) string {
		return fmt.Sprintf(`{"name":%q,"kind":"stage","target":"kind","namespace":"ns","kubeconfig":{"name":"KUBECONFIG","ref":%q,"delivery":"file"}}`, name, ref)
	}
	var seen []string
	check := func(what string, r resp) {
		t.Helper()
		all := r.Body.String() + fmt.Sprint(r.Header())
		seen = append(seen, all)
		if strings.Contains(all, sentinel) || strings.Contains(strings.ToLower(all), "decrypt failed") {
			t.Errorf("%s discloses a credential or a dependency's error text: %s", what, all)
		}
	}

	created := send("POST", "/environments", env("stage", "secret://project/kubeconfig"))
	check("environment creation", created)
	if created.Code != http.StatusCreated {
		t.Fatalf("%d %s", created.Code, created.Body.String())
	}
	var e deliveryhttp.Environment
	created.decode(t, &e)
	if e.Kubeconfig.Ref != "secret://project/kubeconfig" || len(e.Secrets) != 0 {
		t.Fatalf("the environment is a reference, never a value: %+v", e)
	}
	check("environment read", send("GET", "/environments/"+e.Id, ""))
	check("environment list", send("GET", "/environments", ""))
	check("environment edit", send("PATCH", "/environments/"+e.Id, `{"config":{"REPLICAS":"2"}}`, "If-Match", `"1"`))

	// A dependency whose own error carries the value.
	broken := send("POST", "/environments", env("broken", "secret://project/broken"))
	check("a dependency failure", broken)
	if broken.Code != http.StatusServiceUnavailable {
		t.Fatalf("a broken secrets owner must be unavailable, got %d", broken.Code)
	}
	leaky := send("POST", "/environments", env("leaky", "secret://project/leaky"))
	check("a credential refusal", leaky)
	if leaky.Code != http.StatusUnprocessableEntity || leaky.errCode(t) != "credential_unavailable" {
		t.Fatalf("a refused credential: %d %s", leaky.Code, leaky.Body.String())
	}
	// A client that sends a credential value is refused without echo.
	body := strings.Replace(env("typed", "secret://project/kubeconfig"), `"delivery":"file"`, `"delivery":"file","value":"`+sentinel+`"`, 1)
	check("a value in the request", send("POST", "/environments", body))
	if r := send("POST", "/environments", body); r.Code != http.StatusBadRequest {
		t.Fatalf("a credential value in a definition: %d", r.Code)
	}

	// The stored (replayable) response of a keyed creation is the receipt's metadata
	// only: it validates against the schema that forbids additional properties.
	create := `{"attemptId":"att_t035_deploy","source":{"repositoryUrl":"https://git.t035.invalid/acme/inventory.git","commit":"` + strings.Repeat("1", 40) + `"},"image":{"artifactId":"art_t035_image","digest":"` + string(digestA) + `"}}`
	// Drive the keyed render directly through the controlled service: the body
	// handed to the idempotency store is what a replay would release.
	a2 := newAPI(t)
	r := a2.human("POST", pubBase+"/deployments", create, "Idempotency-Key", "d-2")
	if r.Code != http.StatusCreated {
		t.Fatalf("%d %s", r.Code, r.Body.String())
	}
	if err := sp.validate(t, "DeploymentReceipt", r.Body.Bytes()); err != nil {
		t.Fatalf("the stored receipt carries more than metadata: %v", err)
	}
	check("a stored response", r)

	// Logs: operation names, outcomes and identifiers only.
	for _, log := range []string{logs.String(), a.logs.String(), a2.logs.String()} {
		if strings.Contains(log, sentinel) || strings.Contains(strings.ToLower(log), "decrypt failed") {
			t.Errorf("a log line carries a credential or a dependency's error text:\n%s", log)
		}
	}
	if !strings.Contains(logs.String(), "createEnvironment") {
		t.Errorf("the use cases logged nothing: %q", logs.String())
	}
	if len(seen) == 0 {
		t.Fatal("nothing was checked")
	}
}
