{{/*
Expand the name of the chart.
*/}}
{{- define "varnish-gateway.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "varnish-gateway.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "varnish-gateway.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "varnish-gateway.labels" -}}
helm.sh/chart: {{ include "varnish-gateway.chart" . }}
{{ include "varnish-gateway.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{/*
Selector labels for operator
*/}}
{{- define "varnish-gateway.selectorLabels" -}}
app.kubernetes.io/name: varnish-gateway
app.kubernetes.io/component: operator
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Selector labels for chaperone
*/}}
{{- define "varnish-gateway.chaperoneSelectorLabels" -}}
app.kubernetes.io/name: varnish-gateway
app.kubernetes.io/component: chaperone
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "varnish-gateway.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default "varnish-gateway-operator" .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Operator image
*/}}
{{- define "varnish-gateway.operatorImage" -}}
{{- $tag := .Values.operator.image.tag | default .Chart.AppVersion }}
{{- printf "%s:%s" .Values.operator.image.repository $tag }}
{{- end }}

{{/*
Chaperone image
*/}}
{{- define "varnish-gateway.chaperoneImage" -}}
{{- $tag := .Values.chaperone.image.tag | default .Chart.AppVersion }}
{{- printf "%s:%s" .Values.chaperone.image.repository $tag }}
{{- end }}

{{/*
Namespace
*/}}
{{- define "varnish-gateway.namespace" -}}
{{- .Values.namespace | default "varnish-gateway-system" }}
{{- end }}
