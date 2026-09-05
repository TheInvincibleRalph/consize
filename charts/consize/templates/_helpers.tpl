{{/*
Expand the name of the chart.
*/}}
{{- define "consize.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "consize.fullname" -}}
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

{{- define "consize.namespace" -}}
{{- default .Release.Namespace .Values.namespace.name -}}
{{- end -}}

{{- define "consize.labels" -}}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}
app.kubernetes.io/name: {{ include "consize.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{- define "consize.selectorLabels" -}}
app.kubernetes.io/name: {{ include "consize.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "consize.componentLabels" -}}
{{ include "consize.labels" .root }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{- define "consize.readerServiceAccountName" -}}
{{- default (printf "%s-reader" (include "consize.fullname" .)) .Values.serviceAccounts.reader.name -}}
{{- end -}}

{{- define "consize.writerServiceAccountName" -}}
{{- default (printf "%s-writer" (include "consize.fullname" .)) .Values.serviceAccounts.writer.name -}}
{{- end -}}

{{- define "consize.image" -}}
{{- $root := index . 0 -}}
{{- $component := index . 1 -}}
{{- $image := index $root.Values.images $component -}}
{{- $registry := default $root.Values.global.imageRegistry $image.registry -}}
{{- $repository := required (printf "images.%s.repository is required" $component) $image.repository -}}
{{- $tag := default $root.Values.global.imageTag $image.tag -}}
{{- if $registry -}}
{{- printf "%s/%s:%s" $registry $repository $tag -}}
{{- else -}}
{{- printf "%s:%s" $repository $tag -}}
{{- end -}}
{{- end -}}

{{- define "consize.validate" -}}
{{- if and .Values.cloudWaste.enabled (or (eq .Values.cloudWaste.provider "none") (eq .Values.cloudWaste.provider "disabled") (eq .Values.cloudWaste.provider "")) -}}
{{- fail "cloudWaste.enabled=true requires cloudWaste.provider to be gcp, aws, or fixture" -}}
{{- end -}}
{{- if and (eq .Values.cloudWaste.provider "gcp") (not .Values.gcp.project) (not .Values.gcp.credentials.existingSecret) -}}
{{- fail "cloudWaste.provider=gcp requires gcp.project, gcp.credentials.existingSecret, or workload identity/metadata access that can resolve a project" -}}
{{- end -}}
{{- if and (eq .Values.collector.dbMetrics.provider "gcp") (not .Values.gcp.project) (not .Values.gcp.credentials.existingSecret) -}}
{{- fail "collector.dbMetrics.provider=gcp requires gcp.project, gcp.credentials.existingSecret, or workload identity/metadata access that can resolve a project" -}}
{{- end -}}
{{- end -}}

{{- define "consize.databaseEnv" -}}
{{- if .Values.postgresql.enabled }}
- name: POSTGRES_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ include "consize.fullname" . }}-postgresql
      key: postgres-password
- name: DATABASE_URL
  value: postgres://{{ .Values.postgresql.auth.username }}:$(POSTGRES_PASSWORD)@{{ include "consize.fullname" . }}-postgresql:5432/{{ .Values.postgresql.auth.database }}?sslmode=disable
{{- else }}
- name: DATABASE_URL
  valueFrom:
    secretKeyRef:
      name: {{ required "database.existingSecret is required when postgresql.enabled is false" .Values.database.existingSecret }}
      key: {{ .Values.database.keys.databaseUrl }}
{{- end }}
{{- end -}}

{{- define "consize.prometheusEnv" -}}
- name: PROMETHEUS_URL
  valueFrom:
    secretKeyRef:
      name: {{ required "prometheus.existingSecret is required" .Values.prometheus.existingSecret }}
      key: {{ .Values.prometheus.keys.url }}
{{- end -}}

{{- define "consize.gcpEnv" -}}
{{- if .Values.gcp.project }}
- name: CONSIZE_GCP_PROJECT
  value: {{ .Values.gcp.project | quote }}
{{- end }}
{{- if .Values.gcp.credentials.existingSecret }}
- name: GOOGLE_APPLICATION_CREDENTIALS
  value: {{ printf "%s/%s" .Values.gcp.credentials.mountPath .Values.gcp.credentials.key | quote }}
{{- end }}
{{- end -}}

{{- define "consize.awsEnv" -}}
{{- if .Values.aws.region }}
- name: AWS_REGION
  value: {{ .Values.aws.region | quote }}
{{- end }}
{{- end -}}

{{- define "consize.gcpVolume" -}}
{{- if .Values.gcp.credentials.existingSecret }}
volumes:
  - name: consize-gcp
    secret:
      secretName: {{ .Values.gcp.credentials.existingSecret }}
{{- end }}
{{- end -}}

{{- define "consize.gcpVolumeMount" -}}
{{- if .Values.gcp.credentials.existingSecret }}
volumeMounts:
  - mountPath: {{ .Values.gcp.credentials.mountPath }}
    name: consize-gcp
    readOnly: true
{{- end }}
{{- end -}}
