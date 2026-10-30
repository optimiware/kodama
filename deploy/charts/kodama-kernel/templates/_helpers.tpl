{{- define "kodama-kernel.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kodama-kernel.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "kodama-kernel.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "kodama-kernel.labels" -}}
app.kubernetes.io/name: {{ include "kodama-kernel.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "kodama-kernel.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kodama-kernel.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "kodama-kernel.image" -}}
{{- $i := .Values.image -}}
{{- printf "%s:%s@%s" $i.repository ($i.tag | default "latest") $i.digest -}}
{{- end -}}

{{- define "kodama-kernel.checkoutImage" -}}
{{- $i := .Values.execution.checkoutImage -}}
{{- if $i.repository -}}
{{- printf "%s:%s@%s" $i.repository ($i.tag | default "latest") $i.digest -}}
{{- else -}}
{{- include "kodama-kernel.image" . -}}
{{- end -}}
{{- end -}}

{{- define "kodama-kernel.serviceAccount" -}}
{{- printf "%s-server" (include "kodama-kernel.fullname" .) -}}
{{- end -}}

{{- define "kodama-kernel.migrationServiceAccount" -}}
{{- printf "%s-migration" (include "kodama-kernel.fullname" .) -}}
{{- end -}}

{{- define "kodama-kernel.claim" -}}
{{- if .Values.persistence.existingClaim -}}
{{- .Values.persistence.existingClaim -}}
{{- else -}}
{{- printf "%s-data" (include "kodama-kernel.fullname" .) -}}
{{- end -}}
{{- end -}}
