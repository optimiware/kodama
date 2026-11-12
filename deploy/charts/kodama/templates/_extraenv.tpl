{{- /*
config.extraEnv is a narrow pass-through for the optional server variables the
Go composition reads from the environment. It is not a general pod-spec escape
hatch and it never carries a secret:

  * only the allowlisted names below are accepted. The same expression lives in
    values.schema.json (propertyNames); a unit test requires the two to agree;
  * a value is a literal string, never an object, so a value file cannot smuggle
    in valueFrom as "extra environment";
  * a name the chart already renders itself is refused: a duplicate env entry is
    not an error in Kubernetes, the last duplicate wins, and the override would
    silently replace the production composition (profile, TLS dir, database URL,
    ...);
  * development tokens (KODAMA_DEV_*), cloud credentials (SCW_/AWS_/GOOGLE_) and
    credential-shaped names are refused. The only exception is a name ending in
    _FILE (for example KODAMA_OIDC_CA_FILE): that is a mounted path, not a
    credential value;
  * a value containing URL userinfo with a password (scheme://user:password@host)
    is refused, because it would render a literal password into the manifest.

The schema is the first line of defence; this guard repeats every rule so a
rendering path that skipped schema validation still cannot weaken the production
composition.
*/ -}}
{{- define "kodama.validateExtraEnv" -}}
{{- $allowlist := "^(KODAMA_OIDC_(ISSUER|AUDIENCE|JWKS_URL|TENANT_CLAIM|SUBJECT_CLAIM|PROJECT_CLAIM|CA_FILE)|KODAMA_DISCOVERY_(ISSUER|CLIENT_ID|AUDIENCE|AUTHORIZATION_ENDPOINT|TOKEN_ENDPOINT|DEVICE_AUTHORIZATION_ENDPOINT|REVOCATION_ENDPOINT|SCOPES|API_URL)|KODAMA_TENANT_NAMESPACE|KODAMA_WORKLOAD_(CPU|MEMORY)_(REQUEST|LIMIT))$" }}
{{- /* Keep in sync with chartDirectEnvNames in internal/installation. */ -}}
{{- $fixed := list "KODAMA_PROFILE" "KODAMA_INSTALLATION_ID" "KODAMA_PUBLIC_URL" "KODAMA_MCP_PATH" "KODAMA_DATA_DIR" "KODAMA_DATABASE_URL" "KODAMA_CONTROLLER_DATABASE_URL" "KODAMA_PUBLISHER_DATABASE_URL" "KODAMA_AUTH_DATABASE_URL" "KODAMA_NATS_URL" "KODAMA_NATS_CA_FILE" "KODAMA_NATS_CERT_FILE" "KODAMA_NATS_KEY_FILE" "KODAMA_NATS_USER" "KODAMA_NATS_PASSWORD" "KODAMA_NATS_CALLOUT_USER" "KODAMA_NATS_CALLOUT_PASSWORD" "KODAMA_NATS_CALLOUT_ISSUER_SEED" "KODAMA_NATS_CALLOUT_ACCOUNT" "KODAMA_KEY_ADAPTER" "KODAMA_KEY_REF" "KODAMA_KEY_CURRENT_VERSION" "KODAMA_KEY_BLOBS_DIR" "KODAMA_TLS_DIR" "KODAMA_TENANT" "KODAMA_TENANTS" "KODAMA_GIT_STORAGE_ROOT" "KODAMA_WORKSPACE_PVC_STORAGE_CLASS" "KODAMA_WORKSPACE_PVC_SIZE" "KODAMA_WORKSPACE_PVC_ACCESS_MODE" "KODAMA_WORKLOAD_CA_SECRET" }}
{{- /*
The tenant list is the installation's served set: one namespace, RBAC role and
network policy per entry, and KODAMA_TENANTS renders it verbatim. An id that is
not a lowercase DNS label would derive an invalid namespace, a duplicate would
merge two tenants' resources, and a singleton namespace override cannot name a
namespace per tenant. values.schema.json states the same rules.
*/ -}}
{{- if not .Values.tenants }}{{- fail "tenants must not be empty" }}{{- end }}
{{- $tenantNames := dict }}
{{- range $tenant := .Values.tenants }}
{{- $name := toString $tenant }}
{{- if not (regexMatch "^[a-z0-9]([a-z0-9-]{0,47}[a-z0-9])?$" $name) }}
{{- fail (printf "tenants entry %q is not a lowercase DNS label; the chart derives the namespace kodama-tenant-<tenant> from it" $name) }}
{{- end }}
{{- if hasKey $tenantNames $name }}
{{- fail (printf "tenants repeats %q; one tenant id would own two namespaces and RBAC roles" $name) }}
{{- end }}
{{- $_ := set $tenantNames $name true }}
{{- end }}
{{- if and (gt (len .Values.tenants) 1) .Values.config .Values.config.extraEnv (hasKey .Values.config.extraEnv "KODAMA_TENANT_NAMESPACE") }}
{{- fail "config.extraEnv.KODAMA_TENANT_NAMESPACE is a singleton override; a multi-tenant installation derives one namespace per tenant" }}
{{- end }}
{{- range $name, $value := .Values.config.extraEnv }}
{{- if not (kindIs "string" $value) }}
{{- fail (printf "config.extraEnv.%s must be a string literal value; objects and valueFrom are not supported" $name) }}
{{- end }}
{{- if eq (toString $value) "" }}
{{- fail (printf "config.extraEnv.%s must not be empty; an empty value would render an empty environment variable instead of a named configuration error" $name) }}
{{- end }}
{{- if has $name $fixed }}
{{- fail (printf "config.extraEnv.%s collides with a fixed variable the chart renders; a duplicate env entry would override the production composition" $name) }}
{{- end }}
{{- if hasPrefix "KODAMA_DEV_" $name }}
{{- fail (printf "config.extraEnv.%s must not set a development token; the production composition has no development credentials" $name) }}
{{- end }}
{{- if regexMatch "^(SCW|AWS|GOOGLE)_" $name }}
{{- fail (printf "config.extraEnv.%s must not set a cloud credential; credentials come only from the kodama-kms-credentials Secret (scw-kms) or the ServiceAccount's workload identity (aws-kms, gcp-kms)" $name) }}
{{- end }}
{{- if and (regexMatch "(?i)(token|secret|password|passwd|credential|private|key)" $name) (not (regexMatch "_FILE$" $name)) }}
{{- fail (printf "config.extraEnv.%s looks like a credential name; only a mounted *_FILE path may contain such a word, never a value" $name) }}
{{- end }}
{{- if not (regexMatch $allowlist $name) }}
{{- fail (printf "config.extraEnv.%s is not an allowlisted optional server variable; see deploy/charts/kodama/values.yaml" $name) }}
{{- end }}
{{- if regexMatch "://[^/@]*:[^@]*@" $value }}
{{- fail (printf "config.extraEnv.%s value looks like a URL with inline userinfo credentials; put the secret in a Secret, not in a value" $name) }}
{{- end }}
{{- end }}
{{- /*
A production installation must provide the OIDC configuration
cmd/kodama/production.go refuses to start without, and the cloud KMS credential
of its key adapter (config.keyAdapter):

  * aws-kms authenticates with EKS IRSA and gcp-kms with GKE Workload Identity.
    Neither has a static credential, so the kodama-kms-credentials Secret is
    not required and the Deployment renders no reference to it; a value left
    in secrets.kmsCredentials (the chart default names the Secret) is ignored.
    Instead the ServiceAccount this chart creates must carry
    eks.amazonaws.com/role-arn (aws-kms) or iam.gke.io/gcp-service-account
    (gcp-kms). A ServiceAccount managed outside the chart
    (serviceAccount.create: false) is not checked;
  * every other adapter, which in production means scw-kms, requires the
    kodama-kms-credentials Secret that carries SCW_SECRET_KEY. Only the two
    workload-identity adapters are exempt, so an unknown adapter name cannot
    drop the requirement.

The schema states the same requirements (allOf/if/then) so a renderer that
skips schema validation is caught here as well. Development and test profiles
may omit all of them for a local install.
*/ -}}
{{- if eq .Values.environment "production" }}
{{- $extraEnv := dict }}
{{- if .Values.config }}
{{- if .Values.config.extraEnv }}
{{- $extraEnv = .Values.config.extraEnv }}
{{- end }}
{{- end }}
{{- $required := list "KODAMA_OIDC_ISSUER" "KODAMA_OIDC_AUDIENCE" "KODAMA_OIDC_JWKS_URL" "KODAMA_OIDC_TENANT_CLAIM" }}
{{- $missing := list }}
{{- range $name := $required }}
{{- if not (hasKey $extraEnv $name) }}
{{- $missing = append $missing $name }}
{{- end }}
{{- end }}
{{- if $missing }}
{{- fail (printf "production requires config.extraEnv.%s: cmd/kodama/production.go refuses to start without the OIDC issuer, audience, JWKS URL and tenant claim; set them in the operator extra values (see deploy/README.md)" (join ", config.extraEnv." $missing)) }}
{{- end }}
{{- $adapter := "" }}
{{- if .Values.config }}
{{- $adapter = toString (.Values.config.keyAdapter | default "") }}
{{- end }}
{{- if has $adapter (list "aws-kms" "gcp-kms") }}
{{- $annotation := ternary "eks.amazonaws.com/role-arn" "iam.gke.io/gcp-service-account" (eq $adapter "aws-kms") }}
{{- $identity := ternary "the EKS IRSA role ARN" "the Workload Identity Google service account" (eq $adapter "aws-kms") }}
{{- $create := true }}
{{- $annotations := dict }}
{{- if .Values.serviceAccount }}
{{- if hasKey .Values.serviceAccount "create" }}
{{- $create = .Values.serviceAccount.create }}
{{- end }}
{{- if .Values.serviceAccount.annotations }}
{{- $annotations = .Values.serviceAccount.annotations }}
{{- end }}
{{- end }}
{{- if and $create (not (get $annotations $annotation)) }}
{{- fail (printf "production requires serviceAccount.annotations[%q] (%s) for config.keyAdapter %s: the adapter has no static credential, so without the annotation the pod starts and every KMS decrypt is refused; set serviceAccount.create=false only when the ServiceAccount is managed outside this chart" $annotation $identity $adapter) }}
{{- end }}
{{- else }}
{{- $kmsCredentialsRequired := printf "production requires secrets.kmsCredentials.existingSecret (the kodama-kms-credentials Secret) for config.keyAdapter %q: the scw-kms adapter reads SCW_SECRET_KEY from it, and only aws-kms and gcp-kms run without a static credential" $adapter }}
{{- if not .Values.secrets }}
{{- fail $kmsCredentialsRequired }}
{{- end }}
{{- if not .Values.secrets.kmsCredentials }}
{{- fail $kmsCredentialsRequired }}
{{- end }}
{{- if not .Values.secrets.kmsCredentials.existingSecret }}
{{- fail $kmsCredentialsRequired }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}
