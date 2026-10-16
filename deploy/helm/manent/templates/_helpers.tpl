{{- define "manent.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "manent.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "manent.labels" -}}
app.kubernetes.io/name: {{ include "manent.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{- define "manent.selector" -}}
app.kubernetes.io/name: {{ include "manent.name" .root }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{- define "manent.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "manent.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "manent.image" -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}

{{/* Common variables: database, configuration and pod identity. */}}
{{- define "manent.env" -}}
- name: MANENT_CONFIG
  value: /etc/manent/manent.yml
- name: DBOS_SYSTEM_DATABASE_URL
  valueFrom:
    secretKeyRef:
      name: {{ .Values.database.existingSecret }}
      key: {{ .Values.database.key }}
- name: DBOS__VMID
  valueFrom:
    fieldRef:
      fieldPath: metadata.name
{{- with .Values.extraEnv }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{- define "manent.volumeMounts" -}}
- name: config
  mountPath: /etc/manent
  readOnly: true
- name: work
  mountPath: /var/lib/manent/work
- name: tmp
  mountPath: /tmp
{{- with .Values.extraVolumeMounts }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{- define "manent.volumes" -}}
- name: config
  configMap:
    name: {{ include "manent.fullname" . }}
- name: tmp
  emptyDir: {}
{{- with .Values.extraVolumes }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{- define "manent.probes" -}}
startupProbe:
  httpGet: {path: /healthz, port: {{ .port }}}
  periodSeconds: 5
  failureThreshold: 60
livenessProbe:
  httpGet: {path: /healthz, port: {{ .port }}}
  periodSeconds: 20
readinessProbe:
  httpGet: {path: /readyz, port: {{ .port }}}
  periodSeconds: 10
{{- end -}}
