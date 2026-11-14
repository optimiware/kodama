package cli

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/optimiware/kodama/api"
)

// DeriveOIDCPrincipalID derives the deterministic Kodama human principal ID
// ("oidc-" + lowercase hex of sha256(issuer + "\x00" + subject)[:16]).
func DeriveOIDCPrincipalID(issuer, subject string) (string, error) {
	if issuer == "" {
		return "", fmt.Errorf("OIDC issuer is empty")
	}
	if subject == "" {
		return "", fmt.Errorf("OIDC subject is empty")
	}
	sum := sha256.Sum256([]byte(issuer + "\x00" + subject))
	return "oidc-" + hex.EncodeToString(sum[:16]), nil
}

type whoamiView struct {
	PrincipalID string   `json:"principal_id"`
	Kind        string   `json:"kind,omitempty"`
	TenantID    string   `json:"tenant_id,omitempty"`
	ProjectID   string   `json:"project_id,omitempty"`
	Roles       []string `json:"roles,omitempty"`
	Issuer      string   `json:"issuer,omitempty"`
	Subject     string   `json:"subject,omitempty"`
}

// runWhoami displays the caller's principal identity via principal.whoami, or
// from the local session/JWT claims with --offline. A failed server call is
// reported, never replaced by the local claims.
func runWhoami(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"whoami"})
	}
	var (
		offlineFlag bool
		outputFlag  string
	)
	spec := newArgSpec()
	spec.boolFlag(&offlineFlag, "--offline")
	spec.stringFlag(&outputFlag, "--output")
	positionals, _, err := spec.parse(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if len(positionals) > 0 {
		return usage(ports.Err, fmt.Sprintf("unexpected argument %q", positionals[0]))
	}
	format, err := resolveOutputFormat(outputFlag, config)
	if err != nil {
		return usage(ports.Err, err.Error())
	}

	offlineView, hasOffline := deriveOfflineWhoami(ctx, config, ports, deps, offlineFlag)

	if !offlineFlag {
		if err := config.Validate(); err != nil {
			return usage(ports.Err, err.Error())
		}
		if ports.Client == nil {
			return failure(ports.Err, fmt.Errorf("no public client is configured"))
		}
		inputPayload := json.RawMessage(`{}`)
		if operationRequiresRouteParam("principal.whoami", "project") {
			if proj := resolveProject("", config, deps); proj != "" {
				encoded, marshalErr := json.Marshal(map[string]string{"project_id": proj})
				if marshalErr == nil {
					inputPayload = encoded
				}
			}
		}
		invocation := api.Invocation{
			Operation: "principal.whoami",
			Request:   api.Request{Input: inputPayload},
		}
		if err := invocation.Validate(); err != nil {
			return usage(ports.Err, publicMessage(err))
		}
		result, err := ports.Client.Invoke(ctx, invocation)
		if err != nil {
			return failure(ports.Err, err)
		}
		if err := renderWhoamiServerResult(ports.Out, format, result, offlineView); err != nil {
			return failure(ports.Err, err)
		}
		return ExitSuccess
	}

	if !hasOffline {
		return failure(ports.Err, fmt.Errorf("cannot determine principal identity: no active OIDC session with iss/sub claims found (run 'kodactl login')"))
	}
	if format == OutputJSON {
		if err := json.NewEncoder(ports.Out).Encode(offlineView); err != nil {
			return failure(ports.Err, err)
		}
		return ExitSuccess
	}
	writeWhoamiTable(ports.Out, offlineView)
	return ExitSuccess
}

func renderWhoamiServerResult(out interface{ Write([]byte) (int, error) }, format OutputFormat, result api.Result, fallback whoamiView) error {
	if format == OutputJSON {
		return renderResult(out, OutputJSON, result)
	}
	var raw struct {
		PrincipalID string   `json:"principal_id"`
		ID          string   `json:"id"`
		Kind        string   `json:"kind"`
		TenantID    string   `json:"tenant_id"`
		ProjectID   string   `json:"project_id"`
		Roles       []string `json:"roles"`
		Principal   *struct {
			ID   string `json:"id"`
			Kind string `json:"kind"`
		} `json:"principal"`
		Scope *struct {
			TenantID  string `json:"tenant_id"`
			ProjectID string `json:"project_id"`
		} `json:"scope"`
	}
	if len(result.Body) > 0 {
		if err := json.Unmarshal(result.Body, &raw); err != nil {
			return renderTable(out, result)
		}
	}
	view := whoamiView{
		PrincipalID: raw.PrincipalID,
		Kind:        raw.Kind,
		TenantID:    raw.TenantID,
		ProjectID:   raw.ProjectID,
		Roles:       raw.Roles,
	}
	if view.PrincipalID == "" && raw.ID != "" {
		view.PrincipalID = raw.ID
	}
	if raw.Principal != nil {
		if view.PrincipalID == "" {
			view.PrincipalID = raw.Principal.ID
		}
		if view.Kind == "" {
			view.Kind = raw.Principal.Kind
		}
	}
	if raw.Scope != nil {
		if view.TenantID == "" {
			view.TenantID = raw.Scope.TenantID
		}
		if view.ProjectID == "" {
			view.ProjectID = raw.Scope.ProjectID
		}
	}
	if view.PrincipalID == "" && fallback.PrincipalID != "" {
		view.PrincipalID = fallback.PrincipalID
	}
	if view.PrincipalID == "" {
		return renderTable(out, result)
	}
	writeWhoamiTable(out, view)
	return nil
}

func writeWhoamiTable(out interface{ Write([]byte) (int, error) }, view whoamiView) {
	fmt.Fprintf(out, "principal_id\t%s\n", view.PrincipalID)
	if view.Kind != "" {
		fmt.Fprintf(out, "kind\t%s\n", view.Kind)
	}
	if view.TenantID != "" {
		fmt.Fprintf(out, "tenant_id\t%s\n", view.TenantID)
	}
	if view.ProjectID != "" {
		fmt.Fprintf(out, "project_id\t%s\n", view.ProjectID)
	}
	if view.Roles != nil {
		if len(view.Roles) == 0 {
			fmt.Fprintln(out, "roles\t-")
		} else {
			fmt.Fprintf(out, "roles\t%s\n", strings.Join(view.Roles, ","))
		}
	}
	if view.Issuer != "" {
		fmt.Fprintf(out, "issuer\t%s\n", view.Issuer)
	}
	if view.Subject != "" {
		fmt.Fprintf(out, "subject\t%s\n", view.Subject)
	}
}

func deriveOfflineWhoami(ctx context.Context, config Config, ports Ports, deps CommandDeps, offlineOnly bool) (whoamiView, bool) {
	if deps.Login.Store != nil {
		if record, err := deps.Login.Store.LoadRecord(ctx); err == nil {
			iss, sub, ok := parseJWTIssuerSubject(record.Token)
			if iss == "" {
				iss = strings.TrimSpace(record.Issuer)
			}
			if ok && iss != "" && sub != "" {
				if id, deriveErr := DeriveOIDCPrincipalID(iss, sub); deriveErr == nil {
					return whoamiView{
						PrincipalID: id,
						Kind:        "human",
						Issuer:      iss,
						Subject:     sub,
					}, true
				}
			}
		}
	}
	if ports.Session != nil {
		if cred, err := ports.Session.Load(ctx); err == nil {
			if iss, sub, ok := parseJWTIssuerSubject(cred.Token); ok && iss != "" && sub != "" {
				if id, deriveErr := DeriveOIDCPrincipalID(iss, sub); deriveErr == nil {
					return whoamiView{
						PrincipalID: id,
						Kind:        "human",
						Issuer:      iss,
						Subject:     sub,
					}, true
				}
			}
		}
	}
	if config.CredentialFile != "" && ports.Files != nil {
		if data, err := ports.Files.ReadFile(config.CredentialFile); err == nil {
			if iss, sub, ok := parseJWTIssuerSubject(strings.TrimSpace(string(data))); ok && iss != "" && sub != "" {
				if id, deriveErr := DeriveOIDCPrincipalID(iss, sub); deriveErr == nil {
					return whoamiView{
						PrincipalID: id,
						Kind:        "human",
						Issuer:      iss,
						Subject:     sub,
					}, true
				}
			}
		}
	}
	if !offlineOnly && ports.Credential != nil {
		if cred, err := ports.Credential.Credential(ctx); err == nil {
			if iss, sub, ok := parseJWTIssuerSubject(cred.Token); ok && iss != "" && sub != "" {
				if id, deriveErr := DeriveOIDCPrincipalID(iss, sub); deriveErr == nil {
					return whoamiView{
						PrincipalID: id,
						Kind:        "human",
						Issuer:      iss,
						Subject:     sub,
					}, true
				}
			}
		}
	}
	return whoamiView{}, false
}

// parseJWTIssuerSubject decodes the payload claims of a compact JWT to extract
// iss and sub for local principal-ID derivation.
func parseJWTIssuerSubject(token string) (string, string, bool) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 || parts[1] == "" {
		return "", "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return "", "", false
		}
	}
	var claims struct {
		Issuer  string `json:"iss"`
		Subject string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", "", false
	}
	iss := strings.TrimSpace(claims.Issuer)
	sub := strings.TrimSpace(claims.Subject)
	if iss == "" && sub == "" {
		return "", "", false
	}
	return iss, sub, true
}

func operationRequiresRouteParam(operation, paramName string) bool {
	for _, codec := range api.OperationCodecs() {
		if codec.Operation != operation {
			continue
		}
		for _, p := range codec.RouteParams {
			if p.Name == paramName {
				return true
			}
		}
	}
	return false
}

// runMember dispatches `kodactl member <list|grant|revoke>`.
func runMember(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if len(args) == 0 {
		return usage(ports.Err, "member requires a subcommand (list|grant|revoke)")
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		return runHelp(ports.Out, ports.Err, append([]string{"member"}, args[1:]...))
	}
	switch args[0] {
	case "list", "ls":
		return runMemberList(ctx, config, args[1:], ports, deps)
	case "grant":
		return runMemberGrant(ctx, config, args[1:], ports, deps)
	case "revoke":
		return runMemberRevoke(ctx, config, args[1:], ports, deps)
	default:
		return usage(ports.Err, fmt.Sprintf("unknown member subcommand %q", args[0]))
	}
}

func runMemberList(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"member", "list"})
	}
	var (
		projectFlag string
		outputFlag  string
	)
	spec := newArgSpec()
	spec.stringFlag(&projectFlag, "--project", "-p")
	spec.stringFlag(&outputFlag, "--output")
	positionals, _, err := spec.parse(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if len(positionals) > 0 {
		return usage(ports.Err, fmt.Sprintf("unexpected argument %q", positionals[0]))
	}
	format, err := resolveOutputFormat(outputFlag, config)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if err := config.Validate(); err != nil {
		return usage(ports.Err, err.Error())
	}
	projectID := resolveProject(projectFlag, config, deps)
	if projectID == "" {
		return usage(ports.Err, "member list requires --project <id> or KODAMA_PROJECT")
	}
	if err := (api.MembershipListRequest{ProjectID: projectID}).Validate(); err != nil {
		return usage(ports.Err, publicMessage(err))
	}
	if ports.Client == nil {
		return failure(ports.Err, fmt.Errorf("no public client is configured"))
	}

	memberships, firstBody, pages, err := fetchAllMemberships(ctx, ports, projectID)
	if err != nil {
		return failure(ports.Err, err)
	}
	if format == OutputJSON {
		if pages == 1 {
			if err := renderResult(ports.Out, OutputJSON, api.Result{Operation: "membership.list", Body: firstBody}); err != nil {
				return failure(ports.Err, err)
			}
			return ExitSuccess
		}
		if memberships == nil {
			memberships = []api.MembershipView{}
		}
		combined, err := json.Marshal(api.MembershipListResult{Memberships: memberships})
		if err != nil {
			return failure(ports.Err, err)
		}
		if err := renderResult(ports.Out, OutputJSON, api.Result{Operation: "membership.list", Body: combined}); err != nil {
			return failure(ports.Err, err)
		}
		return ExitSuccess
	}

	tw := tabwriter.NewWriter(ports.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PRINCIPAL\tROLES\tVERSION")
	for _, member := range memberships {
		fmt.Fprintf(tw, "%s\t%s\t%d\n",
			member.PrincipalID,
			strings.Join(member.Roles, ","),
			member.Version,
		)
	}
	if err := tw.Flush(); err != nil {
		return failure(ports.Err, err)
	}
	return ExitSuccess
}

func runMemberGrant(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"member", "grant"})
	}
	var (
		projectFlag        string
		subjectFlag        string
		githubFlag         string
		issuerFlag         string
		expectedVerRaw     string
		outputFlag         string
		idempotencyKeyFlag string
		roleFlags          []string
	)
	spec := newArgSpec()
	spec.stringFlag(&projectFlag, "--project", "-p")
	spec.stringFlag(&subjectFlag, "--subject")
	spec.stringFlag(&githubFlag, "--github")
	spec.stringFlag(&issuerFlag, "--issuer")
	spec.listFlag(&roleFlags, "--role", "--roles", "-r")
	spec.stringFlag(&expectedVerRaw, "--expected-version")
	spec.stringFlag(&outputFlag, "--output")
	spec.stringFlag(&idempotencyKeyFlag, "--idempotency-key")
	positionals, seen, err := spec.parse(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if len(positionals) > 1 {
		return usage(ports.Err, fmt.Sprintf("unexpected extra argument %q", positionals[1]))
	}
	format, err := resolveOutputFormat(outputFlag, config)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	expectedVer, err := parsePositiveVersion(expectedVerRaw, "--expected-version")
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if err := config.Validate(); err != nil {
		return usage(ports.Err, err.Error())
	}
	projectID := resolveProject(projectFlag, config, deps)
	if projectID == "" {
		return usage(ports.Err, "member grant requires --project <id> or KODAMA_PROJECT")
	}
	roles := parseRoleList(roleFlags)
	if len(roles) == 0 {
		return usage(ports.Err, "member grant requires --role <role>[,<role>...]")
	}

	positionalTarget := ""
	if len(positionals) == 1 {
		positionalTarget = positionals[0]
	}
	resolved, statusCode := resolveMemberPrincipal(ctx, config, ports, deps,
		positionalTarget, subjectFlag, githubFlag, issuerFlag,
		seen["--subject"], seen["--github"])
	if statusCode != ExitSuccess {
		return statusCode
	}

	req := api.MembershipGrantRequest{
		ProjectID:   projectID,
		PrincipalID: resolved.principalID,
		Roles:       roles,
	}
	if err := req.Validate(); err != nil {
		return usage(ports.Err, publicMessage(err))
	}
	if ports.Client == nil {
		return failure(ports.Err, fmt.Errorf("no public client is configured"))
	}

	input, err := json.Marshal(req)
	if err != nil {
		return failure(ports.Err, err)
	}
	key, err := makeIdempotencyKey(idempotencyKeyFlag, "membership.grant", deps.Login.Random)
	if err != nil {
		return failure(ports.Err, err)
	}
	invocation := api.Invocation{
		Operation: "membership.grant",
		Request: api.Request{
			Input:           input,
			ExpectedVersion: expectedVer,
			IdempotencyKey:  key,
		},
	}
	if err := invocation.Validate(); err != nil {
		return usage(ports.Err, publicMessage(err))
	}
	result, err := ports.Client.Invoke(ctx, invocation)
	if err != nil {
		return failure(ports.Err, err)
	}
	var grantRes api.MembershipGrantResult
	_ = json.Unmarshal(result.Body, &grantRes)

	if format == OutputJSON {
		out := map[string]any{
			"principal_id": resolved.principalID,
			"project_id":   projectID,
			"roles":        roles,
			"version":      grantRes.Version,
		}
		if resolved.derived {
			out["issuer"] = resolved.issuer
			out["subject"] = resolved.subject
		}
		if err := json.NewEncoder(ports.Out).Encode(out); err != nil {
			return failure(ports.Err, err)
		}
		return ExitSuccess
	}

	if resolved.derived {
		fmt.Fprintf(ports.Out, "derived principal_id\t%s\nissuer\t%s\nsubject\t%s\n",
			resolved.principalID, resolved.issuer, resolved.subject)
	} else {
		fmt.Fprintf(ports.Out, "principal_id\t%s\n", resolved.principalID)
	}
	fmt.Fprintf(ports.Out, "project_id\t%s\nroles\t%s\nversion\t%d\n",
		projectID, strings.Join(roles, ","), grantRes.Version)
	return ExitSuccess
}

func runMemberRevoke(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"member", "revoke"})
	}
	var (
		projectFlag        string
		subjectFlag        string
		githubFlag         string
		issuerFlag         string
		expectedVerRaw     string
		outputFlag         string
		idempotencyKeyFlag string
	)
	spec := newArgSpec()
	spec.stringFlag(&projectFlag, "--project", "-p")
	spec.stringFlag(&subjectFlag, "--subject")
	spec.stringFlag(&githubFlag, "--github")
	spec.stringFlag(&issuerFlag, "--issuer")
	spec.stringFlag(&expectedVerRaw, "--expected-version")
	spec.stringFlag(&outputFlag, "--output")
	spec.stringFlag(&idempotencyKeyFlag, "--idempotency-key")
	positionals, seen, err := spec.parse(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if len(positionals) > 1 {
		return usage(ports.Err, fmt.Sprintf("unexpected extra argument %q", positionals[1]))
	}
	format, err := resolveOutputFormat(outputFlag, config)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	expectedVer, err := parsePositiveVersion(expectedVerRaw, "--expected-version")
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if err := config.Validate(); err != nil {
		return usage(ports.Err, err.Error())
	}
	projectID := resolveProject(projectFlag, config, deps)
	if projectID == "" {
		return usage(ports.Err, "member revoke requires --project <id> or KODAMA_PROJECT")
	}

	positionalTarget := ""
	if len(positionals) == 1 {
		positionalTarget = positionals[0]
	}
	resolved, statusCode := resolveMemberPrincipal(ctx, config, ports, deps,
		positionalTarget, subjectFlag, githubFlag, issuerFlag,
		seen["--subject"], seen["--github"])
	if statusCode != ExitSuccess {
		return statusCode
	}

	req := api.MembershipRevokeRequest{
		ProjectID:   projectID,
		PrincipalID: resolved.principalID,
	}
	if err := req.Validate(); err != nil {
		return usage(ports.Err, publicMessage(err))
	}
	if ports.Client == nil {
		return failure(ports.Err, fmt.Errorf("no public client is configured"))
	}

	if expectedVer == nil {
		memberships, _, _, listErr := fetchAllMemberships(ctx, ports, projectID)
		if listErr != nil {
			return failure(ports.Err, listErr)
		}
		var found uint64
		for _, member := range memberships {
			if member.PrincipalID == resolved.principalID {
				found = member.Version
				break
			}
		}
		if found == 0 {
			return failure(ports.Err, fmt.Errorf("principal %q is not a member of project %q", resolved.principalID, projectID))
		}
		expectedVer = &found
	}

	input, err := json.Marshal(req)
	if err != nil {
		return failure(ports.Err, err)
	}
	key, err := makeIdempotencyKey(idempotencyKeyFlag, "membership.revoke", deps.Login.Random)
	if err != nil {
		return failure(ports.Err, err)
	}
	invocation := api.Invocation{
		Operation: "membership.revoke",
		Request: api.Request{
			Input:           input,
			ExpectedVersion: expectedVer,
			IdempotencyKey:  key,
		},
	}
	if err := invocation.Validate(); err != nil {
		return usage(ports.Err, publicMessage(err))
	}
	result, err := ports.Client.Invoke(ctx, invocation)
	if err != nil {
		return failure(ports.Err, err)
	}
	var revokeRes api.MembershipRevokeResult
	_ = json.Unmarshal(result.Body, &revokeRes)

	if format == OutputJSON {
		out := map[string]any{
			"principal_id": resolved.principalID,
			"project_id":   projectID,
			"version":      revokeRes.Version,
		}
		if resolved.derived {
			out["issuer"] = resolved.issuer
			out["subject"] = resolved.subject
		}
		if err := json.NewEncoder(ports.Out).Encode(out); err != nil {
			return failure(ports.Err, err)
		}
		return ExitSuccess
	}

	if resolved.derived {
		fmt.Fprintf(ports.Out, "derived principal_id\t%s\n", resolved.principalID)
	}
	fmt.Fprintf(ports.Out, "revoked %s from project %s (version=%d)\n",
		resolved.principalID, projectID, revokeRes.Version)
	return ExitSuccess
}

type resolvedPrincipal struct {
	principalID string
	issuer      string
	subject     string
	derived     bool
}

func resolveMemberPrincipal(
	ctx context.Context,
	config Config,
	ports Ports,
	deps CommandDeps,
	positional, subjectFlag, githubFlag, issuerFlag string,
	hasSubjectFlag, hasGithubFlag bool,
) (resolvedPrincipal, int) {
	sources := 0
	if strings.TrimSpace(positional) != "" {
		sources++
	}
	if hasSubjectFlag {
		sources++
	}
	if hasGithubFlag {
		sources++
	}
	if sources == 0 {
		return resolvedPrincipal{}, usage(ports.Err, "specify <principal-or-subject>, --subject <sub>, or --github <login>")
	}
	if sources > 1 {
		return resolvedPrincipal{}, usage(ports.Err, "specify only one of <principal-or-subject>, --subject, or --github")
	}

	var subject string
	switch {
	case hasGithubFlag:
		login := strings.ToLower(strings.TrimSpace(githubFlag))
		if login == "" {
			return resolvedPrincipal{}, usage(ports.Err, "--github requires a non-empty login")
		}
		subject = "github:" + login
	case hasSubjectFlag:
		subject = strings.TrimSpace(subjectFlag)
		if subject == "" {
			return resolvedPrincipal{}, usage(ports.Err, "--subject requires a non-empty subject")
		}
	default:
		target := strings.TrimSpace(positional)
		if strings.HasPrefix(target, "oidc-") {
			return resolvedPrincipal{principalID: target}, ExitSuccess
		}
		subject = target
	}

	issuer, err := resolveOIDCIssuer(ctx, config, ports, deps, issuerFlag)
	if err != nil {
		return resolvedPrincipal{}, failure(ports.Err, err)
	}
	derivedID, err := DeriveOIDCPrincipalID(issuer, subject)
	if err != nil {
		return resolvedPrincipal{}, usage(ports.Err, err.Error())
	}
	return resolvedPrincipal{
		principalID: derivedID,
		issuer:      issuer,
		subject:     subject,
		derived:     true,
	}, ExitSuccess
}

// resolveOIDCIssuer resolves the installation's OIDC issuer via explicit flag,
// injected expectation, /.well-known/kodama-configuration discovery, or the
// persisted human session / JWT.
func resolveOIDCIssuer(ctx context.Context, config Config, ports Ports, deps CommandDeps, explicit string) (string, error) {
	if trimmed := strings.TrimSpace(explicit); trimmed != "" {
		return trimmed, nil
	}
	if trimmed := strings.TrimSpace(deps.Login.Issuer); trimmed != "" {
		return trimmed, nil
	}
	var discoveryErr error
	if apiURL, err := normalizeAPIURL(config.ServerURL); err == nil {
		oidcClient := deps.Login.OIDC
		if oidcClient == nil && deps.HTTP != nil {
			oidcClient = &HTTPOIDCClient{RoundTripper: deps.HTTP}
		}
		if oidcClient != nil {
			doc, discErr := oidcClient.Discover(ctx, apiURL+api.DiscoveryPath)
			if discErr == nil && strings.TrimSpace(doc.Issuer) != "" {
				return strings.TrimSpace(doc.Issuer), nil
			}
			discoveryErr = discErr
		}
	}
	if deps.Login.Store != nil {
		if record, err := deps.Login.Store.LoadRecord(ctx); err == nil {
			if iss := strings.TrimSpace(record.Issuer); iss != "" {
				return iss, nil
			}
			if iss, _, ok := parseJWTIssuerSubject(record.Token); ok && iss != "" {
				return iss, nil
			}
		}
	}
	if ports.Credential != nil {
		if cred, err := ports.Credential.Credential(ctx); err == nil {
			if iss, _, ok := parseJWTIssuerSubject(cred.Token); ok && iss != "" {
				return iss, nil
			}
		}
	}
	if discoveryErr != nil {
		return "", fmt.Errorf("cannot resolve OIDC issuer from %s or current session: %w", api.DiscoveryPath, discoveryErr)
	}
	return "", fmt.Errorf("cannot resolve OIDC issuer: configure discovery (%s) or sign in with 'kodactl login'", api.DiscoveryPath)
}

func fetchAllMemberships(ctx context.Context, ports Ports, projectID string) ([]api.MembershipView, json.RawMessage, int, error) {
	var (
		all       []api.MembershipView
		firstBody json.RawMessage
		pages     int
		cursor    string
	)
	for {
		req := api.MembershipListRequest{ProjectID: projectID}
		if cursor != "" {
			req.Page = &api.Page{Cursor: cursor}
		}
		input, err := json.Marshal(req)
		if err != nil {
			return nil, nil, 0, err
		}
		invocation := api.Invocation{
			Operation: "membership.list",
			Request:   api.Request{Input: input},
		}
		if err := invocation.Validate(); err != nil {
			return nil, nil, 0, err
		}
		result, err := ports.Client.Invoke(ctx, invocation)
		if err != nil {
			return nil, nil, 0, err
		}
		pages++
		if pages == 1 {
			firstBody = result.Body
		}
		var page api.MembershipListResult
		if len(result.Body) > 0 {
			if err := json.Unmarshal(result.Body, &page); err != nil {
				return nil, nil, 0, fmt.Errorf("invalid membership.list response: %w", err)
			}
		}
		all = append(all, page.Memberships...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return all, firstBody, pages, nil
}

func parseRoleList(rawFlags []string) []string {
	var roles []string
	seen := make(map[string]bool)
	for _, raw := range rawFlags {
		for _, item := range strings.Split(raw, ",") {
			trimmed := strings.TrimSpace(item)
			if trimmed == "" || seen[trimmed] {
				continue
			}
			seen[trimmed] = true
			roles = append(roles, trimmed)
		}
	}
	return roles
}
