{{- define "pgoverlay.fullname" -}}
{{- if contains "pgoverlay" .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-pgoverlay" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "pgoverlay.labels" -}}
app.kubernetes.io/name: pgoverlay
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{- define "pgoverlay.selectorLabels" -}}
app.kubernetes.io/name: pgoverlay
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* Whether branchd's state dir is a PVC ("true"/"false" string).
     persistence.enabled is tri-state: "" = auto (on with storage.mode=csi,
     off with hostpath), "true"/"false" = explicit override — so an explicit
     false with csi stays false. */}}
{{- define "pgoverlay.persistenceEnabled" -}}
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
{{- define "pgoverlay.leaderElectionEnabled" -}}
{{- if or .Values.leaderElection.enabled (gt (int .Values.replicaCount) 1) -}}
true
{{- else -}}
false
{{- end -}}
{{- end -}}

{{/* Secret holding the API bearer token (key "token"). */}}
{{- define "pgoverlay.tokenSecretName" -}}
{{- .Values.existingSecret | default (printf "%s-token" (include "pgoverlay.fullname" .)) -}}
{{- end -}}

{{/* ghook (GitHub webhook service) naming: distinct selector labels so the
     branchd api/proxy Services never match ghook pods. */}}
{{- define "pgoverlay.ghook.fullname" -}}
{{- printf "%s-ghook" (include "pgoverlay.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "pgoverlay.ghook.selectorLabels" -}}
app.kubernetes.io/name: pgoverlay-ghook
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* Secret holding the webhook HMAC secret (key "webhook-secret") and the
     optional GitHub token (key "github-token"). */}}
{{- define "pgoverlay.ghook.secretName" -}}
{{- .Values.ghook.existingSecret | default (include "pgoverlay.ghook.fullname" .) -}}
{{- end -}}
