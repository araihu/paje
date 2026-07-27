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

{{- define "paje.hatchetSecretName" -}}
{{- default (printf "%s-hatchet" (include "paje.fullname" .)) .Values.secrets.hatchet.existingSecret }}
{{- end }}

{{- define "paje.mem0SecretName" -}}
{{- default (printf "%s-mem0" (include "paje.fullname" .)) .Values.secrets.mem0.existingSecret }}
{{- end }}

{{- define "paje.githubSecretName" -}}
{{- default (printf "%s-github" (include "paje.fullname" .)) .Values.secrets.github.existingSecret }}
{{- end }}

{{- define "paje.pvcName" -}}
{{- default (include "paje.fullname" .) .Values.persistence.existingClaim }}
{{- end }}
