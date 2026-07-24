{{- define "paje.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "paje.fullname" -}}
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

{{- define "paje.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "paje.labels" -}}
helm.sh/chart: {{ include "paje.chart" . }}
{{ include "paje.selectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "paje.selectorLabels" -}}
app.kubernetes.io/name: {{ include "paje.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "paje.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "paje.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "paje.secretName" -}}
{{- default (include "paje.fullname" .) .Values.secrets.existingSecret }}
{{- end }}
