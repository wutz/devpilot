{{/* Default name of the chart, overridable via nameOverride. */}}
{{- define "devpilot.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Fully qualified release-scoped name. */}}
{{- define "devpilot.fullname" -}}
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

{{/* Chart label value. */}}
{{- define "devpilot.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Common labels applied to every object. */}}
{{- define "devpilot.labels" -}}
helm.sh/chart: {{ include "devpilot.chart" . }}
{{ include "devpilot.selectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: devpilot
{{- end }}

{{/* Selector labels: immutable, so they never include the chart version. */}}
{{- define "devpilot.selectorLabels" -}}
app.kubernetes.io/name: {{ include "devpilot.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/* Controller manager service account name. */}}
{{- define "devpilot.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (printf "%s-controller-manager" (include "devpilot.fullname" .)) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/* Image reference and pull policy. */}}
{{- define "devpilot.image" -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) }}
{{- end }}

{{/* Rendered "storageClass=snapshotClass,..." mapping for online commit defaults. */}}
{{- define "devpilot.defaultSnapshotClasses" -}}
{{- $pairs := list -}}
{{- range $sc, $snap := .Values.defaults.snapshotClassByStorageClass -}}
{{- $pairs = append $pairs (printf "%s=%s" $sc $snap) -}}
{{- end -}}
{{- join "," $pairs -}}
{{- end }}

{{/*
Port of the web console, taken from the tail of dashboard.bindAddress so the
Service can never target a port the manager does not listen on. Fails the
render instead of guessing when the address carries no usable port.
*/}}
{{- define "devpilot.dashboardPort" -}}
{{- $port := last (splitList ":" .Values.dashboard.bindAddress) -}}
{{- if not (regexMatch "^[0-9]+$" $port) -}}
{{- fail (printf "dashboard.bindAddress %q has no numeric port" .Values.dashboard.bindAddress) -}}
{{- end -}}
{{- $port -}}
{{- end }}
