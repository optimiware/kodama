package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
)

// pinnedContext is the wrapper's version-independent reading of the attempt's
// pinned context: what it checks against the payload, which outputs the step
// declared, the work item for the prompt and, under version 3, the source set the
// attempt works in. It is read-only evidence from the control plane; nothing in
// it is a credential.
type pinnedContext struct {
	// Version is the runtime context version: 1, 2 or 3.
	Version        int64
	AttemptID      string
	DesignBaseline *runtimev1.DesignBaseline
	Profile        *runtimev1.Profile
	Outputs        []runtimev1.OutputDecl
	WorkItem       *runtimev1.WorkItem
	// V3 is the whole context when Version is 3.
	V3 *runtimev3.ContextV3
}

// route is one runtime version's context route. A run is pinned to one version
// at admission and served only on the matching wire: another version's route
// answers 409 context_version_mismatch without side effects, so the wrapper tries
// the versions in order, the original one first (a v1 attempt costs the one
// request it always did).
type route struct {
	version int64
	prefix  string
	decode  func([]byte) (pinnedContext, error)
}

var contextRoutes = []route{
	{1, "/runtime/v1/attempts/", func(b []byte) (pinnedContext, error) {
		c, err := runtimev1.DecodeRequest[runtimev1.Context](b)
		if err != nil {
			return pinnedContext{}, err
		}
		return pinnedContext{Version: 1, AttemptID: string(c.AttemptID), DesignBaseline: c.DesignBaseline, Profile: c.Profile, Outputs: c.Outputs, WorkItem: c.WorkItem}, nil
	}},
	{3, "/runtime/v3/attempts/", func(b []byte) (pinnedContext, error) {
		c, err := runtimev3.DecodeRequest[runtimev3.ContextV3](b)
		if err != nil {
			return pinnedContext{}, err
		}
		return pinnedContext{Version: 3, AttemptID: string(c.AttemptID), DesignBaseline: c.DesignBaseline, Profile: c.Profile, Outputs: c.Outputs, WorkItem: c.WorkItem, V3: &c}, nil
	}},
	{2, "/runtime/v2/attempts/", func(b []byte) (pinnedContext, error) {
		c, err := runtimev2.DecodeRequest[runtimev2.ContextV2](b)
		if err != nil {
			return pinnedContext{}, err
		}
		return pinnedContext{Version: 2, AttemptID: string(c.AttemptID), DesignBaseline: c.DesignBaseline, Profile: c.Profile, Outputs: c.Outputs, WorkItem: c.WorkItem}, nil
	}},
}

// fetchContext reads the attempt's pinned context with the current token, from
// the route of the version the attempt is pinned to. TLS is verified against the
// platform CA, there are no redirects and no environment proxy.
func fetchContext(ctx context.Context, platform, attempt, tokenFile, caFile string) (pinnedContext, error) {
	tok, err := os.ReadFile(tokenFile)
	if err != nil || strings.TrimSpace(string(tok)) == "" {
		return pinnedContext{}, errors.New("no runtime token")
	}
	pool := x509.NewCertPool()
	pem, err := os.ReadFile(caFile)
	if err != nil || !pool.AppendCertsFromPEM(pem) {
		return pinnedContext{}, errors.New("the platform CA is unreadable")
	}
	hc := &http.Client{
		Timeout:       30 * time.Second,
		Transport:     &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	for _, rt := range contextRoutes {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(platform, "/")+rt.prefix+attempt+"/context", nil)
		if err != nil {
			return pinnedContext{}, err
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
		resp, err := hc.Do(req)
		if err != nil {
			return pinnedContext{}, err
		}
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusConflict {
			continue // pinned to another version: try the next route
		}
		if rerr != nil || resp.StatusCode != http.StatusOK {
			return pinnedContext{}, fmt.Errorf("context request: HTTP %d", resp.StatusCode)
		}
		pc, err := rt.decode(body)
		if err != nil {
			return pinnedContext{}, errors.New("the context is not valid")
		}
		return pc, nil
	}
	return pinnedContext{}, errors.New("context request: HTTP 409 on every runtime version")
}
