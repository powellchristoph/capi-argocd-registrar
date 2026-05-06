{{- define "capi-argocd-registrar.fullname" -}}
{{- printf "%s" .Release.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "capi-argocd-registrar.labels" -}}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{ include "capi-argocd-registrar.selectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "capi-argocd-registrar.selectorLabels" -}}
app.kubernetes.io/name: capi-argocd-registrar
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "capi-argocd-registrar.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "capi-argocd-registrar.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}
