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

{{- define "paje.validateCredentialSecretSeparation" -}}
{{- $hatchet := include "paje.hatchetSecretName" . -}}
{{- if eq .Values.adapters.memory "mem0" -}}
{{- $mem0 := include "paje.mem0SecretName" . -}}
{{- if eq $hatchet $mem0 -}}
{{- fail (printf "active credentials must use distinct Secrets: Hatchet and Mem0 both reference %q" $hatchet) -}}
{{- end -}}
{{- end -}}
{{- if eq .Values.publisher.adapter "github" -}}
{{- $github := include "paje.githubSecretName" . -}}
{{- if eq $hatchet $github -}}
{{- fail (printf "active credentials must use distinct Secrets: Hatchet and GitHub both reference %q" $hatchet) -}}
{{- end -}}
{{- if eq .Values.adapters.memory "mem0" -}}
{{- $mem0 := include "paje.mem0SecretName" . -}}
{{- if eq $mem0 $github -}}
{{- fail (printf "active credentials must use distinct Secrets: Mem0 and GitHub both reference %q" $mem0) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if eq .Values.adapters.runner "codex" -}}
{{- $codex := required "codexAuth.existingSecret is required when adapters.runner=codex" .Values.codexAuth.existingSecret -}}
{{- if eq $hatchet $codex -}}
{{- fail (printf "active credentials must use distinct Secrets: Hatchet and Codex both reference %q" $hatchet) -}}
{{- end -}}
{{- if eq .Values.adapters.memory "mem0" -}}
{{- $mem0 := include "paje.mem0SecretName" . -}}
{{- if eq $mem0 $codex -}}
{{- fail (printf "active credentials must use distinct Secrets: Mem0 and Codex both reference %q" $mem0) -}}
{{- end -}}
{{- end -}}
{{- if eq .Values.publisher.adapter "github" -}}
{{- $github := include "paje.githubSecretName" . -}}
{{- if eq $github $codex -}}
{{- fail (printf "active credentials must use distinct Secrets: GitHub and Codex both reference %q" $github) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end }}

{{- define "paje.pvcName" -}}
{{- default (include "paje.fullname" .) .Values.persistence.existingClaim }}
{{- end }}
