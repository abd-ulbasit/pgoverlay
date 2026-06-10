{{- define "pgbranch.fullname" -}}
{{- if contains "pgbranch" .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-pgbranch" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "pgbranch.labels" -}}
app.kubernetes.io/name: pgbranch
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{- define "pgbranch.selectorLabels" -}}
app.kubernetes.io/name: pgbranch
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* Secret holding the API bearer token (key "token"). */}}
{{- define "pgbranch.tokenSecretName" -}}
{{- .Values.existingSecret | default (printf "%s-token" (include "pgbranch.fullname" .)) -}}
{{- end -}}
