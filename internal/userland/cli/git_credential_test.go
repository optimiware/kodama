package cli_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodema/client"
	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/userland/cli"
)

// TestRunHelperGetSuccess pins the exact Git credential-helper response for a
// fresh human session and for an opaque mounted execution token.
func TestRunHelperGetSuccess(t *testing.T) {
	cases := []struct {
		name       string
		credential client.Credential
	}{
		{name: "human session", credential: humanCredential()},
		{name: "execution token", credential: client.Credential{
			Kind:  client.KindExecution,
			Token: "kodex.exec-1.opaque",
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			source := &scriptedSource{credentials: []client.Credential{testCase.credential}}
			ports, out, errOut := testPorts(&scriptedTransport{})
			ports.Credential = source
			code := cli.Run(context.Background(), testConfig(), []string{
				"git", "credential", "get", "https://git.example.com/shop/repo.git",
			}, ports)
			if code != cli.ExitSuccess {
				t.Fatalf("exit code = %d; want %d (stderr: %s)", code, cli.ExitSuccess, errOut.String())
			}
			want := "username=kodema\npassword=" + testCase.credential.Token + "\n"
			if out.String() != want {
				t.Fatalf("stdout = %q; want %q", out.String(), want)
			}
			if errOut.Len() != 0 {
				t.Fatalf("stderr = %q; want empty", errOut.String())
			}
		})
	}
}

// TestRunHelperRefusals pins empty refusals for service credentials,
// store/erase, unregistered or confused targets, expired credentials and a
// failed source.
func TestRunHelperRefusals(t *testing.T) {
	expired := humanCredential()
	expired.ExpiresAt = testNow().Add(-time.Second)
	cases := []struct {
		name       string
		protocol   string
		url        string
		credential *client.Credential
		sourceErr  error
		wantCalls  int // -1 means the read order is not part of the contract
	}{
		{name: "service credential", protocol: "get", url: "https://git.example.com/shop/repo.git",
			credential: &client.Credential{Kind: client.KindService, Token: "kodex.service-1.sig", ExpiresAt: testNow().Add(time.Hour)}, wantCalls: 1},
		{name: "store", protocol: "store", url: "https://git.example.com/shop/repo.git", wantCalls: 0},
		{name: "erase", protocol: "erase", url: "https://git.example.com/shop/repo.git", wantCalls: 0},
		{name: "unregistered target", protocol: "get", url: "https://git.example.com/other/repo.git",
			credential: &client.Credential{Kind: client.KindSession, Token: "human-token", ExpiresAt: testNow().Add(time.Hour)}, wantCalls: -1},
		{name: "path confusion", protocol: "get", url: "https://git.example.com/shopkeeper",
			credential: &client.Credential{Kind: client.KindSession, Token: "human-token", ExpiresAt: testNow().Add(time.Hour)}, wantCalls: -1},
		{name: "host confusion", protocol: "get", url: "https://git.example.com.evil/shop/repo.git",
			credential: &client.Credential{Kind: client.KindSession, Token: "human-token", ExpiresAt: testNow().Add(time.Hour)}, wantCalls: -1},
		{name: "insecure scheme", protocol: "get", url: "http://git.example.com/shop/repo.git",
			credential: &client.Credential{Kind: client.KindSession, Token: "human-token", ExpiresAt: testNow().Add(time.Hour)}, wantCalls: -1},
		{name: "userinfo", protocol: "get", url: "https://user@git.example.com/shop/repo.git",
			credential: &client.Credential{Kind: client.KindSession, Token: "human-token", ExpiresAt: testNow().Add(time.Hour)}, wantCalls: -1},
		{name: "encoded traversal", protocol: "get", url: "https://git.example.com/shop/%2e%2e/secret",
			credential: &client.Credential{Kind: client.KindSession, Token: "human-token", ExpiresAt: testNow().Add(time.Hour)}, wantCalls: -1},
		{name: "expired credential", protocol: "get", url: "https://git.example.com/shop/repo.git",
			credential: &expired, wantCalls: -1},
		{name: "source error", protocol: "get", url: "https://git.example.com/shop/repo.git",
			sourceErr: fault.New(fault.Unauthenticated, "no session"), wantCalls: -1},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			source := &scriptedSource{err: testCase.sourceErr}
			if testCase.credential != nil {
				source.credentials = []client.Credential{*testCase.credential}
			}
			ports, out, errOut := testPorts(&scriptedTransport{})
			ports.Credential = source
			code := cli.Run(context.Background(), testConfig(), []string{
				"git", "credential", testCase.protocol, testCase.url,
			}, ports)
			if code != cli.ExitSuccess {
				t.Fatalf("exit code = %d; want %d", code, cli.ExitSuccess)
			}
			if out.Len() != 0 {
				t.Fatalf("stdout = %q; want an empty refusal", out.String())
			}
			if errOut.Len() != 0 {
				t.Fatalf("stderr = %q; want empty", errOut.String())
			}
			if testCase.wantCalls >= 0 {
				if got := source.callCount(); got != testCase.wantCalls {
					t.Fatalf("credential reads = %d; want %d", got, testCase.wantCalls)
				}
			}
		})
	}
}

// TestRunHelperReadsFreshToken pins that the helper reads the credential source
// on every invocation, so a rotated human or execution token takes effect.
func TestRunHelperReadsFreshToken(t *testing.T) {
	source := &scriptedSource{credentials: []client.Credential{
		{Kind: client.KindSession, Token: "kodex.human-1.first", ExpiresAt: testNow().Add(time.Hour)},
		{Kind: client.KindExecution, Token: "kodex.exec-1.second"},
	}}
	ports, out, errOut := testPorts(&scriptedTransport{})
	ports.Credential = source
	args := []string{"git", "credential", "get", "https://git.example.com/shop/repo.git"}

	if code := cli.Run(context.Background(), testConfig(), args, ports); code != cli.ExitSuccess {
		t.Fatalf("first exit code = %d", code)
	}
	first := out.String()
	out.Reset()
	if code := cli.Run(context.Background(), testConfig(), args, ports); code != cli.ExitSuccess {
		t.Fatalf("second exit code = %d", code)
	}
	second := out.String()
	if first == second {
		t.Fatalf("helper reused the first token: %q", first)
	}
	if !strings.Contains(first, "kodex.human-1.first") || !strings.Contains(second, "kodex.exec-1.second") {
		t.Fatalf("helper tokens = %q, %q; want the fresh token each call", first, second)
	}
	if source.callCount() != 2 {
		t.Fatalf("credential reads = %d; want 2", source.callCount())
	}
	if errOut.Len() != 0 {
		t.Fatalf("stderr = %q; want empty", errOut.String())
	}
}

// TestRunHelperUsageErrors pins invalid helper syntax and that no credential
// reaches stderr.
func TestRunHelperUsageErrors(t *testing.T) {
	source := &scriptedSource{credentials: []client.Credential{humanCredential()}}
	cases := [][]string{
		{"git", "credential"},
		{"git", "credential", "get"},
		{"git", "credential", "bogus", "https://git.example.com/shop/repo.git"},
		{"git", "credential", "get", "not a url"},
	}
	for _, args := range cases {
		ports, out, errOut := testPorts(&scriptedTransport{})
		ports.Credential = source
		code := cli.Run(context.Background(), testConfig(), args, ports)
		if code != cli.ExitUsage {
			t.Errorf("args %v: exit code = %d; want %d", args, code, cli.ExitUsage)
		}
		if out.Len() != 0 {
			t.Errorf("args %v: stdout = %q; want empty", args, out.String())
		}
		if errOut.Len() == 0 {
			t.Errorf("args %v: stderr is empty; want a usage diagnostic", args)
		}
		if strings.Contains(errOut.String(), humanCredential().Token) {
			t.Errorf("args %v: stderr leaked the token: %s", args, errOut.String())
		}
	}
}

// TestGitCredentialReadsGitsRequest pins the protocol Git itself speaks: the
// operation is the argument and the request attributes arrive on standard
// input, up to the first empty line.
func TestGitCredentialReadsGitsRequest(t *testing.T) {
	answer := "username=kodema\npassword=" + humanCredential().Token + "\n"
	for _, testCase := range []struct {
		name  string
		stdin string
		want  string
	}{
		{name: "a trusted origin and path", stdin: "protocol=https\nhost=git.example.com\npath=shop/repo.git\n\n", want: answer},
		{name: "attributes after the empty line are not read", stdin: "protocol=https\nhost=git.example.com\npath=shop/repo.git\n\nhost=evil.example\n", want: answer},
		{name: "attributes Git adds are ignored", stdin: "capability[]=authtype\nprotocol=https\nhost=git.example.com\npath=shop/repo.git\nwwwauth[]=Basic realm=\"kodema\"\n", want: answer},
		{name: "another host", stdin: "protocol=https\nhost=evil.example\npath=shop/repo.git\n\n"},
		{name: "a path outside the trusted one", stdin: "protocol=https\nhost=git.example.com\npath=shopkeeper\n\n"},
		{name: "no path, when the trusted origin has one", stdin: "protocol=https\nhost=git.example.com\n\n"},
		{name: "an insecure scheme", stdin: "protocol=http\nhost=git.example.com\npath=shop/repo.git\n\n"},
		{name: "no host", stdin: "protocol=https\n\n"},
		{name: "no protocol", stdin: "host=git.example.com\n\n"},
		{name: "a line that is not an attribute", stdin: "protocol=https\nhost=git.example.com\nnonsense\n\n"},
		{name: "a truncated request", stdin: "protocol=https\nhost=git.example.com\npath=shop/repo.git\nextra=" + strings.Repeat("x", 64<<10)},
		{name: "host is not a path", stdin: "protocol=https\nhost=git.example.com/shop\n\n"},
		{name: "an empty request", stdin: ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ports, out, errOut := testPorts(&scriptedTransport{})
			ports.Credential = &scriptedSource{credentials: []client.Credential{humanCredential()}}
			ports.In = strings.NewReader(testCase.stdin)
			code := cli.Run(context.Background(), testConfig(), []string{"git", "credential", "get"}, ports)
			if code != cli.ExitSuccess {
				t.Fatalf("exit code = %d; want %d (stderr: %s)", code, cli.ExitSuccess, errOut.String())
			}
			if out.String() != testCase.want {
				t.Fatalf("stdout = %q; want %q", out.String(), testCase.want)
			}
			if errOut.Len() != 0 {
				t.Fatalf("stderr = %q; want empty", errOut.String())
			}
		})
	}
}

// TestGitCredentialStoreAndEraseReadNothing pins that Git's store and erase
// calls succeed without reading the request or the credential source.
func TestGitCredentialStoreAndEraseReadNothing(t *testing.T) {
	for _, operation := range []string{"store", "erase"} {
		source := &scriptedSource{credentials: []client.Credential{humanCredential()}}
		ports, out, errOut := testPorts(&scriptedTransport{})
		ports.Credential = source
		ports.In = strings.NewReader("protocol=https\nhost=git.example.com\npath=shop/repo.git\nusername=kodema\npassword=secret\n\n")
		if code := cli.Run(context.Background(), testConfig(), []string{"git", "credential", operation}, ports); code != cli.ExitSuccess {
			t.Fatalf("%s exit code = %d; stderr: %s", operation, code, errOut.String())
		}
		if out.Len() != 0 || errOut.Len() != 0 || source.callCount() != 0 {
			t.Fatalf("%s wrote %q/%q and read the credential %d time(s); want nothing", operation, out.String(), errOut.String(), source.callCount())
		}
	}
}
