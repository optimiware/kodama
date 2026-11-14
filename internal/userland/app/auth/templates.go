package auth

import (
	"bytes"
	"html/template"
	"net/http"
)

type personaOption struct {
	Name        string
	PrincipalID string
	Selected    bool
}

type loginPageData struct {
	Issuer              string
	ClientID            string
	TenantID            string
	TxID                string
	CSRFToken           string
	ErrorMessage        string
	GitHubEnabled       bool
	GitHubSetupEnabled  bool
	UpstreamOIDCEnabled bool
	UpstreamOIDCName    string
	LocalEnabled        bool
	Personas            []personaOption
	RequireAccessCode   bool
}

type devicePageData struct {
	Issuer              string
	TenantID            string
	CSRFToken           string
	UserCode            string
	ErrorMessage        string
	GitHubEnabled       bool
	GitHubSetupEnabled  bool
	UpstreamOIDCEnabled bool
	UpstreamOIDCName    string
	LocalEnabled        bool
	Personas            []personaOption
	RequireAccessCode   bool
}

type githubSetupPageData struct {
	Issuer            string
	TxID              string
	CSRFToken         string
	Org               string
	Locked            bool
	ErrorMessage      string
	ActionURL         string
	ManifestJSON      string
	SetupFormEndpoint string
}

type statusPageData struct {
	Title       string
	Heading     string
	Message     string
	PrincipalID string
	Subject     string
	ContinueURL string
	ContinueLbl string
	IsError     bool
}

const sharedStyles = `
:root {
  color-scheme: light dark;
  --bg: #f6f7f9;
  --card: #ffffff;
  --fg: #111827;
  --muted: #4b5563;
  --border: #d1d5db;
  --accent: #111827;
  --accent-fg: #ffffff;
  --code-bg: #f3f4f6;
  --err-bg: #fef2f2;
  --err-fg: #991b1b;
  --err-border: #fca5a5;
  --ok-bg: #ecfdf5;
  --ok-fg: #065f46;
}
@media (prefers-color-scheme: dark) {
  :root {
    --bg: #0b0f17;
    --card: #131a26;
    --fg: #f3f4f6;
    --muted: #9ca3af;
    --border: #2a3444;
    --accent: #f3f4f6;
    --accent-fg: #111827;
    --code-bg: #1c2536;
    --err-bg: #2c1518;
    --err-fg: #fca5a5;
    --err-border: #7f1d1d;
    --ok-bg: #0f2922;
    --ok-fg: #6ee7b7;
  }
}
* { box-sizing: border-box; }
body {
  margin: 0;
  min-height: 100vh;
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 24px 16px;
  font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
  background: var(--bg);
  color: var(--fg);
  line-height: 1.45;
}
main {
  width: 100%;
  max-width: 440px;
  background: var(--card);
  border: 1px solid var(--border);
  border-radius: 12px;
  padding: 28px;
}
h1 { margin: 0 0 6px; font-size: 20px; font-weight: 600; }
p.sub { margin: 0 0 20px; font-size: 13px; color: var(--muted); }
.alert {
  padding: 10px 12px;
  border-radius: 8px;
  border: 1px solid var(--err-border);
  background: var(--err-bg);
  color: var(--err-fg);
  font-size: 13px;
  margin-bottom: 16px;
}
.btn {
  display: flex;
  align-items: center;
  justify-content: center;
  width: 100%;
  padding: 10px 14px;
  border-radius: 8px;
  border: 1px solid var(--border);
  background: var(--card);
  color: var(--fg);
  font-size: 14px;
  font-weight: 500;
  text-decoration: none;
  cursor: pointer;
}
.btn-primary {
  background: var(--accent);
  color: var(--accent-fg);
  border-color: var(--accent);
}
.stack { display: flex; flex-direction: column; gap: 10px; }
.divider {
  display: flex;
  align-items: center;
  gap: 10px;
  margin: 18px 0;
  color: var(--muted);
  font-size: 12px;
}
.divider::before, .divider::after {
  content: "";
  flex: 1;
  height: 1px;
  background: var(--border);
}
label.field { display: block; font-size: 13px; font-weight: 500; margin-bottom: 6px; }
input[type="text"], input[type="password"] {
  width: 100%;
  padding: 9px 11px;
  border-radius: 8px;
  border: 1px solid var(--border);
  background: var(--bg);
  color: var(--fg);
  font-size: 14px;
  margin-bottom: 12px;
}
.persona-list { display: flex; flex-direction: column; gap: 8px; margin-bottom: 14px; }
.persona-item {
  display: flex;
  align-items: flex-start;
  gap: 10px;
  padding: 10px 12px;
  border: 1px solid var(--border);
  border-radius: 8px;
  cursor: pointer;
}
.persona-meta { display: flex; flex-direction: column; gap: 2px; min-width: 0; }
.persona-name { font-size: 14px; font-weight: 600; }
code.pid {
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 11.5px;
  background: var(--code-bg);
  padding: 2px 6px;
  border-radius: 4px;
  word-break: break-all;
}
.meta-box {
  margin-top: 14px;
  padding: 12px;
  border-radius: 8px;
  background: var(--ok-bg);
  color: var(--ok-fg);
  font-size: 13px;
}
`

var (
	loginTmpl = template.Must(template.New("login").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Sign in to Kodama</title>
<style>` + sharedStyles + `</style>
</head>
<body>
<main>
  <h1>Sign in to Kodama</h1>
  <p class="sub">Client <strong>{{.ClientID}}</strong> &middot; Tenant <strong>{{.TenantID}}</strong></p>
  {{if .ErrorMessage}}<div class="alert" role="alert">{{.ErrorMessage}}</div>{{end}}

  {{if or .GitHubEnabled .GitHubSetupEnabled .UpstreamOIDCEnabled}}
  <div class="stack">
    {{if .GitHubEnabled}}
    <form method="POST" action="/oauth/github/start">
      <input type="hidden" name="tx_id" value="{{.TxID}}">
      <input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
      <button class="btn btn-primary" type="submit">Continue with GitHub</button>
    </form>
    {{else if .GitHubSetupEnabled}}
    <a class="btn" href="/setup/github?tx={{.TxID}}">Configure GitHub Login (1-Click)</a>
    {{end}}

    {{if .UpstreamOIDCEnabled}}
    <form method="POST" action="/oauth/oidc/start">
      <input type="hidden" name="tx_id" value="{{.TxID}}">
      <input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
      <button class="btn{{if not .GitHubEnabled}} btn-primary{{end}}" type="submit">Continue with {{.UpstreamOIDCName}}</button>
    </form>
    {{end}}
  </div>
  {{end}}

  {{if and .LocalEnabled (or .GitHubEnabled .GitHubSetupEnabled .UpstreamOIDCEnabled)}}
  <div class="divider">or local persona</div>
  {{end}}

  {{if .LocalEnabled}}
  <form method="POST" action="/authorize">
    <input type="hidden" name="tx_id" value="{{.TxID}}">
    <input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
    <div class="persona-list">
      {{range .Personas}}
      <label class="persona-item">
        <input type="radio" name="persona" value="{{.Name}}" {{if .Selected}}checked{{end}} required>
        <span class="persona-meta">
          <span class="persona-name">{{.Name}}</span>
          <code class="pid">{{.PrincipalID}}</code>
        </span>
      </label>
      {{end}}
    </div>
    {{if .RequireAccessCode}}
    <label class="field" for="access_code">Access code</label>
    <input id="access_code" name="access_code" type="password" autocomplete="current-password" required>
    {{end}}
    <button class="btn btn-primary" type="submit">Sign in locally</button>
  </form>
  {{end}}
</main>
</body>
</html>
`))

	deviceTmpl = template.Must(template.New("device").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Authorize Kodama Device</title>
<style>` + sharedStyles + `</style>
</head>
<body>
<main>
  <h1>Authorize Device</h1>
  <p class="sub">Enter the code displayed in your terminal &middot; Tenant <strong>{{.TenantID}}</strong></p>
  {{if .ErrorMessage}}<div class="alert" role="alert">{{.ErrorMessage}}</div>{{end}}

  <form method="POST" action="/device/verify">
    <input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
    <label class="field" for="user_code">Device code</label>
    <input id="user_code" name="user_code" type="text" value="{{.UserCode}}" placeholder="XXXX-XXXX" autocomplete="off" required>

    {{if or .GitHubEnabled .UpstreamOIDCEnabled}}
    <div class="stack" style="margin-bottom: 12px;">
      {{if .GitHubEnabled}}
      <button class="btn btn-primary" type="submit" name="method" value="github">Continue with GitHub</button>
      {{end}}
      {{if .UpstreamOIDCEnabled}}
      <button class="btn" type="submit" name="method" value="oidc">Continue with {{.UpstreamOIDCName}}</button>
      {{end}}
    </div>
    {{else if .GitHubSetupEnabled}}
    <div class="stack" style="margin-bottom: 12px;">
      <a class="btn" href="/setup/github">Configure GitHub Login (1-Click)</a>
    </div>
    {{end}}

    {{if and .LocalEnabled (or .GitHubEnabled .UpstreamOIDCEnabled)}}
    <div class="divider">or local persona</div>
    {{end}}

    {{if .LocalEnabled}}
    <div class="persona-list">
      {{range .Personas}}
      <label class="persona-item">
        <input type="radio" name="persona" value="{{.Name}}" {{if .Selected}}checked{{end}}>
        <span class="persona-meta">
          <span class="persona-name">{{.Name}}</span>
          <code class="pid">{{.PrincipalID}}</code>
        </span>
      </label>
      {{end}}
    </div>
    {{if .RequireAccessCode}}
    <label class="field" for="access_code">Access code</label>
    <input id="access_code" name="access_code" type="password" autocomplete="current-password">
    {{end}}
    <div class="stack">
      <button class="btn btn-primary" type="submit" name="method" value="local">Authorize device</button>
      <button class="btn" type="submit" name="action" value="deny">Deny request</button>
    </div>
    {{else}}
    <button class="btn" type="submit" name="action" value="deny">Deny request</button>
    {{end}}
  </form>
</main>
</body>
</html>
`))

	githubSetupTmpl = template.Must(template.New("github_setup").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Configure GitHub Login</title>
<style>` + sharedStyles + `</style>
</head>
<body>
<main>
  <h1>Configure GitHub Login</h1>
  <p class="sub">Register a GitHub App for <strong>{{.Issuer}}</strong> in one click via the GitHub App Manifest flow.</p>
  {{if .ErrorMessage}}<div class="alert" role="alert">{{.ErrorMessage}}</div>{{end}}

  {{if .Locked}}
  <form method="POST" action="{{.SetupFormEndpoint}}">
    <input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
    <input type="hidden" name="tx" value="{{.TxID}}">
    <label class="field" for="org">GitHub Organization (optional)</label>
    <input id="org" name="org" type="text" value="{{.Org}}" placeholder="Leave blank for personal account">
    <label class="field" for="access_code">Access code</label>
    <input id="access_code" name="access_code" type="password" autocomplete="current-password" required>
    <button class="btn btn-primary" type="submit">Unlock 1-Click Setup</button>
  </form>
  {{else}}
  <form method="POST" action="{{.ActionURL}}">
    <input type="hidden" name="manifest" value="{{.ManifestJSON}}">
    <button class="btn btn-primary" type="submit">Create GitHub App on GitHub</button>
  </form>
  {{end}}
</main>
</body>
</html>
`))

	statusTmpl = template.Must(template.New("status").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>` + sharedStyles + `</style>
</head>
<body>
<main>
  <h1>{{.Heading}}</h1>
  <p class="sub">{{.Message}}</p>
  {{if .PrincipalID}}
  <div class="meta-box">
    <div>Kodama Principal ID:</div>
    <code class="pid">{{.PrincipalID}}</code>
    {{if .Subject}}<div style="margin-top:6px;">Subject: <code class="pid">{{.Subject}}</code></div>{{end}}
  </div>
  {{end}}
  {{if .ContinueURL}}
  <div style="margin-top: 16px;">
    <a class="btn btn-primary" href="{{.ContinueURL}}">{{.ContinueLbl}}</a>
  </div>
  {{end}}
</main>
</body>
</html>
`))
)

func renderHTML(w http.ResponseWriter, status int, tmpl *template.Template, data any) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		http.Error(w, "internal rendering error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}
