{{/*
Fail-closed broker authentication guard for the Kodama stack chart.

KODAMA.md:309 and docs/specs/08-operations-events.md:65 require broker
authentication and authorization on the same credential. The broker therefore
never renders without a client-authentication mechanism:

  * mtls (default): every client presents a certificate signed by the CA in
    nats.auth.mtls.existingSecret, and the server verifies it.
  * credentials: mTLS plus a static NATS user/password authorization block
    whose values come from nats.auth.credentials.existingSecret.
  * callout: mTLS plus per-credential authorization callout. The callout
    service and the control plane use bypass users from
    nats.auth.callout.existingSecret; the account issuer seed and the broker's
    public key are also there. No seed or password is ever rendered inline.

There is no anonymous mode. An empty or unknown configuration aborts the
render instead of producing an open broker.
*/}}
{{- define "kodama-stack.natsAuthGuard" -}}
{{- $auth := .Values.nats.auth | default (dict) -}}
{{- $mode := $auth.mode | default "" -}}
{{- if not (has $mode (list "callout" "mtls" "credentials")) -}}
{{- fail "nats.auth.mode must be \"callout\" (default), \"mtls\" or \"credentials\": anonymous NATS access is forbidden" -}}
{{- end -}}
{{- $mtls := $auth.mtls | default (dict) -}}
{{- if not ($mtls.existingSecret | default "") -}}
{{- fail "nats.auth.mtls.existingSecret is required: the broker verifies client certificates against this CA; anonymous NATS access is forbidden" -}}
{{- end -}}
{{- if eq $mode "credentials" -}}
{{- $credentials := $auth.credentials | default (dict) -}}
{{- if not ($credentials.existingSecret | default "") -}}
{{- fail "nats.auth.credentials.existingSecret is required when nats.auth.mode=credentials" -}}
{{- end -}}
{{- end -}}
{{- if eq $mode "callout" -}}
{{- $callout := $auth.callout | default (dict) -}}
{{- if not ($callout.existingSecret | default "") -}}
{{- fail "nats.auth.callout.existingSecret is required when nats.auth.mode=callout" -}}
{{- end -}}
{{- if not ($callout.account | default "") -}}
{{- fail "nats.auth.callout.account is required when nats.auth.mode=callout" -}}
{{- end -}}
{{- $kodama := .Values.kodama | default (dict) -}}
{{- $secrets := $kodama.secrets | default (dict) -}}
{{- $natsAuth := $secrets.natsAuth | default (dict) -}}
{{- if and ($natsAuth.account | default "") (ne (toString $natsAuth.account) (toString $callout.account)) -}}
{{- fail (printf "kodama.secrets.natsAuth.account (%q) must equal nats.auth.callout.account (%q): the broker account and the callout grant account must be one value" (toString $natsAuth.account) (toString $callout.account)) -}}
{{- end -}}
{{- end -}}
{{- end -}}
