// Login port-shape vectors.
//
// The CLI owns its collaborator ports. These are method-shape vectors, like the
// filesystem seam vector: the production implementations and the test
// fakes must satisfy exactly these consumer-owned declarations.
package publiccontract

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/userland/cli"
)

// The reference fakes are the compile-time witnesses for the declared ports.
var (
	_ cli.BrowserOpener      = (*loginFakeBrowser)(nil)
	_ cli.LoopbackListener   = (*loginFakeLoopback)(nil)
	_ cli.OIDCClient         = (*loginFakeOIDC)(nil)
	_ cli.Clock              = (*loginFakeClock)(nil)
	_ cli.Random             = (*loginFakeRandom)(nil)
	_ cli.SessionLock        = (*loginFakeLock)(nil)
	_ cli.SessionRecordStore = (*loginFakeStore)(nil)
)

// The file-backed store implements both the session store seam and the v2
// record operations.
var (
	_ cli.SessionStore       = cli.FileSessionStore{}
	_ cli.SessionRecordStore = cli.FileSessionStore{}
)

func assertPortShape(t *testing.T, name string, port reflect.Type, want map[string]reflect.Type) {
	t.Helper()
	if port.Kind() != reflect.Interface {
		t.Fatalf("%s is %s; want an interface port", name, port.Kind())
	}
	if port.NumMethod() != len(want) {
		t.Errorf("%s has %d methods; want exactly %d", name, port.NumMethod(), len(want))
	}
	for methodName, wantType := range want {
		method, ok := port.MethodByName(methodName)
		if !ok {
			t.Errorf("%s is missing %s", name, methodName)
			continue
		}
		if method.Type != wantType {
			t.Errorf("%s.%s signature = %v; want %v", name, methodName, method.Type, wantType)
		}
	}
}

// TestLoginPortShapes pins every CLI collaborator port.
func TestLoginPortShapes(t *testing.T) {
	assertPortShape(t, "cli.BrowserOpener", reflect.TypeOf((*cli.BrowserOpener)(nil)).Elem(), map[string]reflect.Type{
		"Open": reflect.TypeOf(func(context.Context, string) error { return nil }),
	})

	assertPortShape(t, "cli.LoopbackListener", reflect.TypeOf((*cli.LoopbackListener)(nil)).Elem(), map[string]reflect.Type{
		"Listen": reflect.TypeOf(func(context.Context) (string, func(context.Context) (cli.CallbackParams, error), func() error, error) {
			return "", nil, nil, nil
		}),
	})

	assertPortShape(t, "cli.OIDCClient", reflect.TypeOf((*cli.OIDCClient)(nil)).Elem(), map[string]reflect.Type{
		"Discover": reflect.TypeOf(func(context.Context, string) (api.DiscoveryDocument, error) {
			return api.DiscoveryDocument{}, nil
		}),
		"Token": reflect.TypeOf(func(context.Context, cli.TokenRequest) (cli.TokenResponse, error) {
			return cli.TokenResponse{}, nil
		}),
		"DeviceAuthorize": reflect.TypeOf(func(context.Context, cli.DeviceAuthorizationRequest) (cli.DeviceAuthorizationResponse, error) {
			return cli.DeviceAuthorizationResponse{}, nil
		}),
		"Revoke": reflect.TypeOf(func(context.Context, cli.RevocationRequest) error { return nil }),
		"JWKS": reflect.TypeOf(func(context.Context, string) (json.RawMessage, error) {
			return nil, nil
		}),
	})

	assertPortShape(t, "cli.Clock", reflect.TypeOf((*cli.Clock)(nil)).Elem(), map[string]reflect.Type{
		"Now":   reflect.TypeOf(func() time.Time { return time.Time{} }),
		"Sleep": reflect.TypeOf(func(context.Context, time.Duration) error { return nil }),
	})

	assertPortShape(t, "cli.Random", reflect.TypeOf((*cli.Random)(nil)).Elem(), map[string]reflect.Type{
		"Read": reflect.TypeOf(func([]byte) (int, error) { return 0, nil }),
	})

	assertPortShape(t, "cli.SessionLock", reflect.TypeOf((*cli.SessionLock)(nil)).Elem(), map[string]reflect.Type{
		"Acquire": reflect.TypeOf(func(context.Context) (func() error, error) { return nil, nil }),
	})

	assertPortShape(t, "cli.SessionRecordStore", reflect.TypeOf((*cli.SessionRecordStore)(nil)).Elem(), map[string]reflect.Type{
		"LoadRecord":   reflect.TypeOf(func(context.Context) (cli.SessionRecord, error) { return cli.SessionRecord{}, nil }),
		"SaveRecord":   reflect.TypeOf(func(context.Context, cli.SessionRecord) error { return nil }),
		"DeleteRecord": reflect.TypeOf(func(context.Context) error { return nil }),
	})

	// The human-session boundary stays available beside the v2 record
	// operations.
	assertPortShape(t, "cli.SessionStore", reflect.TypeOf((*cli.SessionStore)(nil)).Elem(), map[string]reflect.Type{
		"Load": reflect.TypeOf(func(context.Context) (client.Credential, error) { return client.Credential{}, nil }),
		"Save": reflect.TypeOf(func(context.Context, client.Credential) error { return nil }),
	})
}

// TestLoginFlowTypes pins the values the flow exchanges through the ports and
// the fixed callback/session constants.
func TestLoginFlowTypes(t *testing.T) {
	// A callback carries only the four authorization response members and never
	// the PKCE verifier.
	callbackType := reflect.TypeFor[cli.CallbackParams]()
	if callbackType.Kind() != reflect.Struct {
		t.Fatalf("cli.CallbackParams kind = %s; want struct", callbackType.Kind())
	}
	wantFields := []string{"Code", "State", "Error", "ErrorDescription"}
	if callbackType.NumField() != len(wantFields) {
		t.Fatalf("cli.CallbackParams has %d fields; want %d (%v)", callbackType.NumField(), len(wantFields), wantFields)
	}
	stringType := reflect.TypeFor[string]()
	for index, wantName := range wantFields {
		field := callbackType.Field(index)
		if !field.IsExported() {
			t.Fatalf("cli.CallbackParams field %d (%s) is unexported; want exported %s", index, field.Name, wantName)
		}
		if field.Name != wantName {
			t.Fatalf("cli.CallbackParams field %d = %q; want %q", index, field.Name, wantName)
		}
		if field.Type != stringType {
			t.Fatalf("cli.CallbackParams.%s type = %v; want string", field.Name, field.Type)
		}
	}

	if cli.SessionFileVersion != 2 {
		t.Errorf("cli.SessionFileVersion = %d; want the v2 session schema", cli.SessionFileVersion)
	}
	if cli.OAuthCallbackPath != "/callback" {
		t.Errorf("cli.OAuthCallbackPath = %q; want the fixed loopback callback path", cli.OAuthCallbackPath)
	}

	// Discovery and refresh modes are fixed values.
	switch cli.LoginAuto {
	case cli.LoginBrowser, cli.LoginDevice:
		t.Errorf("cli.LoginAuto aliases an explicit mode")
	}
	if cli.LoginBrowser == cli.LoginDevice {
		t.Error("browser and device login modes are the same value")
	}
}
