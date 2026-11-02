{{/*
The run_shared execution policy list rendered into the settings document (O-30; docs/contracts/
run-resources.md). Retained bodies come first, verbatim and in order; the current policy is built from
`runShared.policy` and is always last (the newest is the one new admissions pin). The server decodes the
list strictly into execution.ExecutionPolicy, derives each digest from the canonical JSON and refuses a
repeated id or digest: the list is append-only.
*/}}
{{- define "kodama.runSharedPolicies" -}}
{{- $rs := .Values.runShared -}}
{{- $p := $rs.policy -}}
{{- $prep := $p.roles.preparationImage | default (include "kodama.releaseImage" .) -}}
{{- range $rs.retainedPolicies }}
- {{- toYaml . | nindent 2 }}
{{- end }}
- id: {{ $p.id | quote }}
  admission:
    legacyFormat: {{ $rs.admission.legacyFormat }}
    runShared: {{ $rs.admission.runShared }}
  limits:
    {{- toYaml $p.limits | nindent 4 }}
  requires: {{ if $p.requires }}{{ toJson $p.requires }}{{ else }}[]{{ end }}
  workspace:
    access: ReadWriteOncePod
    reclaim: Delete
    idmapped: {{ $p.workspace.idmapped }}
    storageClass: {{ $p.workspace.storageClass | quote }}
  roles:
    step:
      user: nonroot
      userNamespace: pod
      capabilitiesAdd: {{ toJson $p.roles.step.capabilitiesAdd }}
      seccomp: {{ $p.roles.step.seccomp | quote }}
      lsm: {{ $p.roles.step.lsm | quote }}
      devices: []
      {{- if $p.roles.step.procMount }}
      procMount: {{ $p.roles.step.procMount | quote }}
      {{- end }}
    service:
      user: nonroot
      userNamespace: pod
      capabilitiesAdd: []
      seccomp: RuntimeDefault
      lsm: ""
      devices: []
    builderService:
      user: nonroot
      userNamespace: pod
      capabilitiesAdd: []
      seccomp: RuntimeDefault
      lsm: ""
      devices: []
    preparation:
      user: nonroot
      userNamespace: pod
      capabilitiesAdd: []
      seccomp: RuntimeDefault
      lsm: ""
      devices: []
      image: {{ $prep | quote }}
  podSecurity:
    level: baseline
    version: {{ $rs.podSecurity.version | quote }}
  builders:
    rootless: {{ toJson $p.builders.rootless }}
    service:
      mode: none
  egressProfiles:
    {{- if $p.egressProfiles }}
    {{- toYaml $p.egressProfiles | nindent 4 }}
    {{- else }} {}
    {{- end }}
  network:
    isolation: {{ $p.network.isolation }}
    publicEgress: {{ $p.network.publicEgress }}
{{- end -}}
