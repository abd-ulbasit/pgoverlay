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

{{/* Whether branchd's state dir is a PVC ("true"/"false" string).
     persistence.enabled is tri-state: "" = auto (on with storage.mode=csi,
     off with hostpath), "true"/"false" = explicit override — so an explicit
     false with csi stays false. */}}
{{- define "pgbranch.persistenceEnabled" -}}
{{- $e := .Values.persistence.enabled | toString -}}
{{- if eq $e "" -}}
{{- eq .Values.storage.mode "csi" -}}
{{- else -}}
{{- eq $e "true" -}}
{{- end -}}
{{- end -}}

{{/* Whether leader election is effectively on ("true"/"false" string): when
     leaderElection.enabled OR replicaCount > 1. Running >1 replica without
     leader election would let multiple instances reconcile/write the shared
     registry, so replicas>1 implies it. */}}
{{- define "pgbranch.leaderElectionEnabled" -}}
{{- if or .Values.leaderElection.enabled (gt (int .Values.replicaCount) 1) -}}
true
{{- else -}}
false
{{- end -}}
{{- end -}}

{{/* Secret holding the API bearer token (key "token"). */}}
{{- define "pgbranch.tokenSecretName" -}}
{{- .Values.existingSecret | default (printf "%s-token" (include "pgbranch.fullname" .)) -}}
{{- end -}}

{{/* ghook (GitHub webhook service) naming: distinct selector labels so the
     branchd api/proxy Services never match ghook pods. */}}
{{- define "pgbranch.ghook.fullname" -}}
{{- printf "%s-ghook" (include "pgbranch.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "pgbranch.ghook.selectorLabels" -}}
app.kubernetes.io/name: pgbranch-ghook
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* Secret holding the webhook HMAC secret (key "webhook-secret") and the
     optional GitHub token (key "github-token"). */}}
{{- define "pgbranch.ghook.secretName" -}}
{{- .Values.ghook.existingSecret | default (include "pgbranch.ghook.fullname" .) -}}
{{- end -}}
