{{/*
Expand the name of the chart.
*/}}
{{- define "i2g.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "i2g.fullname" -}}
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
Chart label.
*/}}
{{- define "i2g.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "i2g.labels" -}}
helm.sh/chart: {{ include "i2g.chart" . }}
{{ include "i2g.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "i2g.selectorLabels" -}}
app.kubernetes.io/name: {{ include "i2g.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Name for cluster-scoped resources and resources created outside the release
namespace. Includes the release namespace: Helm release names are only
unique per namespace, so the fullname alone could collide between two
installations of the chart in different namespaces.
*/}}
{{- define "i2g.clusterScopedName" -}}
{{- printf "%s-%s" (include "i2g.fullname" .) .Release.Namespace | trunc 253 | trimSuffix "-" }}
{{- end }}

{{/*
Effective watch namespaces as a comma-joined string. Empty means watching all
namespaces. Defaults to the release namespace in namespaced RBAC mode and
enforces mutual exclusion with the namespace selector.
*/}}
{{- define "i2g.watchNamespaces" -}}
{{- $namespaces := .Values.controller.watchNamespaces }}
{{- if and (not $namespaces) .Values.rbac.namespaced }}
{{- $namespaces = list .Release.Namespace }}
{{- end }}
{{- if and $namespaces .Values.controller.namespaceSelector }}
{{- fail "controller.namespaceSelector cannot be combined with controller.watchNamespaces or rbac.namespaced=true: selecting namespaces by label requires watching all namespaces" }}
{{- end }}
{{- join "," $namespaces }}
{{- end }}

{{/*
Name of the ServiceAccount to use.
*/}}
{{- define "i2g.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "i2g.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}
