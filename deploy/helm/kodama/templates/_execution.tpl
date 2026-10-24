{{/* Command run by the injector init container. Defaults to a plain copy. */}}
{{- define "kodama.injectCommand" -}}
{{- $default := list "cp" .Values.execution.entrypoint.sourcePath "/kodama/bin/kodama" -}}
{{- toYaml (.Values.execution.entrypoint.copyCommand | default $default) -}}
{{- end -}}

{{/*
Restricted tenant Job FIXTURE for T-021. Helm does not create Jobs: the server builds the real
Job at runtime. This shows the shape the tenant namespaces are designed to admit (PX-04, IT-07).
*/}}
{{- define "kodama.exampleJob" -}}
{{- $tenant := dict "namespace" "kodama-tenant-example" -}}
{{- if .Values.execution.tenants -}}{{- $tenant = first .Values.execution.tenants -}}{{- end -}}
{{- $vol := .Values.execution.volumes -}}
# FIXTURE ONLY: rendered for T-021 to consume; Helm never creates this Job.
apiVersion: batch/v1
kind: Job
metadata:
  name: example-attempt
  namespace: {{ $tenant.namespace }}
  labels:
    app.kubernetes.io/part-of: kodama
    app.kubernetes.io/component: execution-example
spec:
  backoffLimit: 0
  activeDeadlineSeconds: {{ .Values.execution.job.activeDeadlineSeconds }}
  template:
    metadata:
      labels:
        app.kubernetes.io/part-of: kodama
        app.kubernetes.io/component: execution-example
    spec:
      restartPolicy: Never
      serviceAccountName: {{ .Values.execution.serviceAccountName }}
      automountServiceAccountToken: false
      enableServiceLinks: false
      securityContext:
        runAsNonRoot: true
        runAsUser: {{ .Values.execution.job.runAsUser }}
        runAsGroup: {{ .Values.execution.job.runAsGroup }}
        fsGroup: {{ .Values.execution.job.runAsGroup }}
        seccompProfile:
          type: RuntimeDefault
      initContainers:
        - name: inject-kodama
          image: {{ include "kodama.releaseImage" . | quote }}
          command:
            {{- include "kodama.injectCommand" . | nindent 12 }}
          resources:
            {{- toYaml .Values.execution.job.injectorResources | nindent 12 }}
          securityContext:
            {{- include "kodama.jobContainerSecurityContext" . | nindent 12 }}
            readOnlyRootFilesystem: true
          volumeMounts:
            - name: tools
              mountPath: /kodama/bin
      containers:
        - name: workload
          image: {{ include "kodama.imageRef" .Values.execution.exampleJob.image | quote }}
          command: ["/kodama/bin/kodama", "step"]
          resources:
            {{- toYaml .Values.execution.job.resources | nindent 12 }}
          securityContext:
            {{- include "kodama.jobContainerSecurityContext" . | nindent 12 }}
          volumeMounts:
            - name: workspace
              mountPath: /workspace
            - name: outputs
              mountPath: /kodama/outputs
            - name: secrets
              mountPath: /kodama/secrets
            - name: identity
              mountPath: /kodama/identity
            - name: tools
              mountPath: /kodama/bin
              readOnly: true
      volumes:
        - name: workspace
          emptyDir:
            sizeLimit: {{ $vol.workspaceSizeLimit }}
        - name: outputs
          emptyDir:
            sizeLimit: {{ $vol.outputsSizeLimit }}
        - name: secrets
          emptyDir:
            medium: Memory
            sizeLimit: {{ $vol.secretsSizeLimit }}
        - name: identity
          emptyDir:
            medium: Memory
            sizeLimit: {{ $vol.identitySizeLimit }}
        - name: tools
          emptyDir:
            sizeLimit: {{ $vol.toolsSizeLimit }}
{{- end -}}
