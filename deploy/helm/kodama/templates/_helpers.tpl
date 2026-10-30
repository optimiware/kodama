{{/* Names */}}
{{- define "kodama.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kodama.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "kodama.serverServiceAccount" -}}
{{- include "kodama.fullname" . -}}
{{- end -}}

{{- define "kodama.migrationServiceAccount" -}}
{{- printf "%s-migrate" (include "kodama.fullname" . | trunc 56 | trimSuffix "-") -}}
{{- end -}}

{{/* Labels */}}
{{- define "kodama.labels" -}}
app.kubernetes.io/name: {{ include "kodama.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: kodama
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end -}}

{{- define "kodama.serverSelectorLabels" -}}
app.kubernetes.io/name: {{ include "kodama.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: server
{{- end -}}

{{- define "kodama.mcpFullname" -}}
{{- printf "%s-mcp" (include "kodama.fullname" . | trunc 58 | trimSuffix "-") -}}
{{- end -}}

{{- define "kodama.mcpServiceAccount" -}}
{{- include "kodama.mcpFullname" . -}}
{{- end -}}

{{- define "kodama.mcpSelectorLabels" -}}
app.kubernetes.io/name: {{ include "kodama.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: mcp
{{- end -}}

{{- define "kodama.mcpAPIURL" -}}
{{- if .Values.mcp.apiURL -}}
{{- .Values.mcp.apiURL -}}
{{- else -}}
{{- printf "http://%s:%d/api/v1" (include "kodama.fullname" .) (int .Values.service.port) -}}
{{- end -}}
{{- end -}}

{{- define "kodama.mcpResource" -}}
{{- if .Values.mcp.resource -}}
{{- .Values.mcp.resource -}}
{{- else if .Values.mcp.ingress.enabled -}}
{{- printf "https://%s/mcp" .Values.mcp.ingress.host -}}
{{- else -}}
{{- printf "http://%s:%d/mcp" (include "kodama.mcpFullname" .) (int .Values.mcp.service.port) -}}
{{- end -}}
{{- end -}}

{{- define "kodama.mcpAudience" -}}
{{- default (include "kodama.mcpResource" .) .Values.mcp.audience -}}
{{- end -}}

{{- define "kodama.mcpAPIAudience" -}}
{{- default .Values.config.oidc.audience .Values.mcp.apiAudience -}}
{{- end -}}

{{- define "kodama.mcpAPIIssuer" -}}
{{- default .Values.config.oidc.issuer .Values.mcp.apiIssuer -}}
{{- end -}}

{{- define "kodama.mcpAPIResource" -}}
{{- default (include "kodama.mcpAPIAudience" .) .Values.mcp.apiResource -}}
{{- end -}}

{{/* Image reference pinned by digest: repository[:tag]@sha256:... */}}
{{- define "kodama.imageRef" -}}
{{- if .tag -}}
{{- printf "%s:%s@%s" .repository .tag .digest -}}
{{- else -}}
{{- printf "%s@%s" .repository .digest -}}
{{- end -}}
{{- end -}}

{{- define "kodama.releaseImage" -}}
{{- include "kodama.imageRef" .Values.image -}}
{{- end -}}

{{/* Restricted container context shared by server and migration containers. */}}
{{- define "kodama.containerSecurityContext" -}}
privileged: {{ .Values.securityContext.privileged }}
allowPrivilegeEscalation: {{ .Values.securityContext.allowPrivilegeEscalation }}
readOnlyRootFilesystem: {{ .Values.securityContext.readOnlyRootFilesystem }}
runAsNonRoot: {{ .Values.securityContext.runAsNonRoot }}
runAsUser: {{ .Values.securityContext.runAsUser }}
runAsGroup: {{ .Values.securityContext.runAsGroup }}
capabilities:
  drop: ["ALL"]
seccompProfile:
  type: RuntimeDefault
{{- end -}}

{{/* Restricted context for containers in tenant execution Pods (workload root FS stays writable). */}}
{{- define "kodama.jobContainerSecurityContext" -}}
privileged: false
allowPrivilegeEscalation: false
runAsNonRoot: true
runAsUser: {{ .Values.execution.job.runAsUser }}
runAsGroup: {{ .Values.execution.job.runAsGroup }}
capabilities:
  drop: ["ALL"]
seccompProfile:
  type: RuntimeDefault
{{- end -}}

{{/* Fail closed on unsupported selections. Included once from the server Deployment. */}}
{{- define "kodama.validate" -}}
{{- if ne .Values.authority.mode "standalone" -}}
{{- fail (printf "authority.mode=%q is unsupported: Optimi-hosted authority is not available until T-061 and never falls back to standalone. Select authority.mode=standalone explicitly." .Values.authority.mode) -}}
{{- end -}}
{{- if and (eq .Values.config.s3.credentials "workloadIdentity") (not .Values.serviceAccounts.server.annotations) -}}
{{- fail "config.s3.credentials=workloadIdentity requires workload-identity annotations in serviceAccounts.server.annotations" -}}
{{- end -}}
{{- range .Values.execution.tenants -}}
{{- if eq .namespace $.Release.Namespace -}}
{{- fail (printf "execution tenant namespace %q must differ from the release namespace" .namespace) -}}
{{- end -}}
{{- end -}}
{{- if and .Values.mcp.enabled .Values.mcp.ingress.enabled (not .Values.mcp.ingress.host) -}}
{{- fail "mcp.ingress.enabled requires mcp.ingress.host" -}}
{{- end -}}
{{- if and .Values.mcp.enabled .Values.mcp.ingress.enabled .Values.ingress.enabled (eq .Values.mcp.ingress.host .Values.ingress.host) (ne .Values.mcp.ingress.path "/mcp") -}}
{{- fail "a shared MCP/server host requires mcp.ingress.path=/mcp; a dedicated host may use /" -}}
{{- end -}}
{{- if and .Values.mcp.enabled (not .Values.mcp.exchange.url) -}}
{{- fail "mcp.enabled requires mcp.exchange.url" -}}
{{- end -}}
{{- if and .Values.mcp.enabled (not .Values.mcp.exchange.clientId) -}}
{{- fail "mcp.enabled requires mcp.exchange.clientId" -}}
{{- end -}}
{{- if and .Values.mcp.enabled (not .Values.mcp.exchange.existingSecret) -}}
{{- fail "mcp.enabled requires mcp.exchange.existingSecret" -}}
{{- end -}}
{{- end -}}

{{/* Name of the selected KMS provider (the schema guarantees exactly one, with its block set). */}}
{{- define "kodama.kmsProvider" -}}
{{- if eq .Values.secrets.adapter "kms" -}}{{ .Values.secrets.kms.provider }}{{- else -}}keyfile{{- end -}}
{{- end -}}

{{/* Secret volume items: only files the selected configuration requires are projected. */}}
{{- define "kodama.runtimeSecretItems" -}}
{{- $items := list "database-runtime-url" "nats-token" "git-read-token" "git-write-token" "git-credentials.json" "cursor-key" "idempotency-hash-key" "runtime-signing-key" "runtime-ca.crt" -}}
{{- if eq .Values.config.s3.credentials "static" -}}
{{- $items = concat $items (list "s3-access-key" "s3-secret-key") -}}
{{- end -}}
{{- if .Values.config.tls.customCA -}}
{{- $items = append $items "ca.crt" -}}
{{- end -}}
{{- if eq .Values.secrets.adapter "keyfile" -}}
{{- $keyfile := .Values.secrets.keyfile | default (dict "files" (dict "v1" "/secrets/master-key")) -}}
{{- range $label, $path := $keyfile.files -}}
{{- $items = append $items (base $path) -}}
{{- end -}}
{{- end -}}
{{- range $items }}
- key: {{ . }}
  path: {{ . }}
{{- end -}}
{{- end -}}
