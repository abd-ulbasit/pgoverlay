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

{{/* Whether branchd must be pinned to .Values.node ("true"/"false" string).
     It must when something it needs lives on that node's disk: all CoW data
     in hostpath mode, and its sqlite state whenever that is a hostPath
     (persistence off). csi mode with the state on a PVC needs no node at all;
     pinning it there would keep branchd Pending forever once that node is
     replaced (an EKS node-group roll), though nothing on it is needed. */}}
{{- define "pgoverlay.pinNode" -}}
{{- if or (eq .Values.storage.mode "hostpath") (ne (include "pgoverlay.persistenceEnabled" .) "true") -}}
true
{{- else -}}
false
{{- end -}}
{{- end -}}

{{/* Pod Security Admission level the release's pods need in this namespace:
     "privileged" when anything uses hostPath volumes or added capabilities
     (hostpath mode: branch pods, helpers and branchd's state; csi without
     persistence: branchd's hostPath state), else "baseline". "restricted" is
     never enough: branchd and the postgres entrypoint start as root. */}}
{{- define "pgoverlay.podSecurityLevel" -}}
{{- if or (eq .Values.storage.mode "hostpath") (ne (include "pgoverlay.persistenceEnabled" .) "true") -}}
privileged
{{- else -}}
baseline
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

{{/* NetworkPolicy peers for cluster DNS: networkPolicy.dnsFrom, or the
     kube-dns pods in any namespace (the CoreDNS convention). */}}
{{- define "pgoverlay.dnsPeers" -}}
{{- with .Values.networkPolicy.dnsFrom -}}
{{- toYaml . -}}
{{- else -}}
- namespaceSelector: {}
  podSelector:
    matchLabels:
      k8s-app: kube-dns
{{- end -}}
{{- end -}}

{{/* Container image reference for an image block (repository, tag, digest).
     A digest wins: the image is pulled as repository@digest and the tag is
     not rendered, because a digest names exactly one image and a tag next to
     it would only suggest a version that might not match. Without a digest
     it is repository:tag, the tag defaulting to the chart's appVersion.
     Called with (dict "image" <block> "appVersion" .Chart.AppVersion
     "name" "<values path, for the error>"). */}}
{{- define "pgoverlay.imageRef" -}}
{{- $img := .image -}}
{{- if $img.digest -}}
{{- if not (regexMatch "^sha256:[a-f0-9]{64}$" $img.digest) -}}
{{- fail (printf "%s.digest must be sha256:<64 lowercase hex characters>, got %q" .name $img.digest) -}}
{{- end -}}
{{- printf "%s@%s" $img.repository $img.digest -}}
{{- else -}}
{{- printf "%s:%s" $img.repository ($img.tag | default .appVersion) -}}
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
