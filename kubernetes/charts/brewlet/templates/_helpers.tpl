{{/* Common helpers for the brewlet chart. */}}

{{- define "brewlet.namespace" -}}
{{- default "brewlet" .Values.namespace -}}
{{- end -}}

{{/* Standard labels applied to every rendered object. */}}
{{- define "brewlet.labels" -}}
app.kubernetes.io/name: brewlet
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: brewlet-{{ .Chart.Version }}
{{- end -}}

{{/* Fully-qualified DNS name of the admission webhook Service. */}}
{{- define "brewlet.admission.serviceName" -}}
brewlet-admission
{{- end -}}

{{/*
brewlet.jdkItems renders either a comma-separated "<dist>-<feature>" JDK string
or a structured JDK list as the NodeProfile spec.jdks list. Call with a dict:
  {{ include "brewlet.jdkItems" (dict "value" "temurin-21,microsoft-25") }}
*/}}
{{- define "brewlet.jdkItems" -}}
{{- if kindIs "string" .value -}}
{{- $spec := trim .value -}}
{{- if not $spec -}}{{- fail "provisioner.jdks must list at least one <dist>-<feature> JDK" -}}{{- end -}}
{{- range $tok := splitList "," $spec -}}
{{- $t := trim $tok -}}
{{- if $t -}}
{{- $parts := splitList "-" $t -}}
{{- if ne (len $parts) 2 -}}{{- fail (printf "invalid JDK token %q; want <distribution>-<feature>" $t) -}}{{- end -}}
- distribution: {{ index $parts 0 | quote }}
  feature: {{ index $parts 1 }}
{{ end -}}
{{- end -}}
{{- else if kindIs "slice" .value -}}
{{- if eq (len .value) 0 -}}{{- fail "provisioner.jdks must list at least one JDK" -}}{{- end -}}
{{- toYaml .value -}}
{{- else -}}
{{- fail "JDK inventory must be a comma-separated string or a list of JDK objects" -}}
{{- end -}}
{{- end -}}

{{/* Render either JDK inventory form as the legacy comma-separated token list. */}}
{{- define "brewlet.jdkTokens" -}}
{{- if kindIs "string" .value -}}
{{- trim .value -}}
{{- else if kindIs "slice" .value -}}
{{- $tokens := list -}}
{{- range $jdk := .value -}}
{{- $tokens = append $tokens (printf "%s-%v" $jdk.distribution $jdk.feature) -}}
{{- end -}}
{{- join "," $tokens -}}
{{- else -}}
{{- fail "JDK inventory must be a comma-separated string or a list of JDK objects" -}}
{{- end -}}
{{- end -}}

{{/*
brewlet.launcherItems renders a comma-separated launcher string as a YAML list.
Emits nothing when empty.
*/}}
{{- define "brewlet.launcherItems" -}}
{{- $spec := trim .value -}}
{{- range $tok := splitList "," $spec -}}
{{- $t := trim $tok -}}
{{- if $t }}
- {{ $t | quote }}
{{- end -}}
{{- end -}}
{{- end -}}
