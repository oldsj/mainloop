{{/* An image reference: repository@digest when a digest is set, else repository:tag. */}}
{{- define "mainloop.image" -}}
{{- if .image.digest -}}
{{ .image.repository }}@{{ .image.digest }}
{{- else -}}
{{ .image.repository }}:{{ .image.tag | default .root.Chart.AppVersion }}
{{- end -}}
{{- end -}}

{{- define "mainloop.backendImage" -}}
{{ include "mainloop.image" (dict "image" .Values.images.backend "root" .) }}
{{- end -}}

{{- define "mainloop.frontendImage" -}}
{{ include "mainloop.image" (dict "image" .Values.images.frontend "root" .) }}
{{- end -}}

{{- define "mainloop.pullPolicy" -}}
{{- with .Values.images.pullPolicy }}
imagePullPolicy: {{ . }}
{{- end }}
{{- end -}}

{{- define "mainloop.dbHost" -}}
{{- if .Values.database.cnpg.enabled -}}
{{ .Values.database.cnpg.name }}-rw.{{ .Release.Namespace }}.svc.cluster.local
{{- else if .Values.database.local.enabled -}}
mainloop-postgres
{{- else -}}
{{ .Values.database.host }}
{{- end -}}
{{- end -}}

{{/* DB_USER and DB_PASSWORD, from the CNPG app Secret or mainloop-secrets unless overridden. */}}
{{- define "mainloop.dbEnv" -}}
{{- $c := .Values.database.credentials -}}
{{- $cnpg := .Values.database.cnpg.enabled -}}
- name: DB_USER
  valueFrom:
    secretKeyRef:
      name: {{ $c.secretName | default (ternary (printf "%s-app" .Values.database.cnpg.name) "mainloop-secrets" $cnpg) }}
      key: {{ $c.usernameKey | default (ternary "username" "db-username" $cnpg) }}
- name: DB_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ $c.secretName | default (ternary (printf "%s-app" .Values.database.cnpg.name) "mainloop-secrets" $cnpg) }}
      key: {{ $c.passwordKey | default (ternary "password" "db-password" $cnpg) }}
{{- end -}}

{{- define "mainloop.securityContext" -}}
securityContext:
  runAsNonRoot: true
  runAsUser: 1000
  runAsGroup: 1000
  allowPrivilegeEscalation: false
  capabilities:
    drop:
      - ALL
  seccompProfile:
    type: RuntimeDefault
{{- end -}}

{{/* envFrom, env and mounts shared by the three listeners. */}}
{{- define "mainloop.serverEnv" -}}
envFrom:
  - configMapRef:
      name: mainloop-config
  {{- if .Values.server.importSecretEnv }}
  - secretRef:
      name: mainloop-secrets
  {{- end }}
  {{- with .Values.server.extraEnvFrom }}
  {{- toYaml . | nindent 2 }}
  {{- end }}
env:
  {{- include "mainloop.dbEnv" . | nindent 2 }}
  - name: AGENT_TOKEN_KEY
    valueFrom:
      secretKeyRef:
        name: mainloop-secrets
        key: agent-token-key
  - name: GITHUB_APP_ID
    valueFrom:
      secretKeyRef:
        name: mainloop-secrets
        key: github-app-id
        {{- if .Values.server.githubAppOptional }}
        optional: true
        {{- end }}
  - name: GITHUB_APP_PRIVATE_KEY
    valueFrom:
      secretKeyRef:
        name: mainloop-secrets
        key: github-app-private-key
        {{- if .Values.server.githubAppOptional }}
        optional: true
        {{- end }}
  {{- if .Values.kagentControlAuth.enabled }}
  - name: KAGENT_GATEWAY_URL
    value: {{ .Values.kagentControlAuth.gatewayURL }}
  - name: KAGENT_USER_ID
    value: {{ .Values.kagentControlAuth.userID }}
  - name: KAGENT_CONTROL_TOKEN_FILE
    value: /run/kagent-control-auth/current
  - name: SSL_CERT_FILE
    value: /run/kagent-control-ca/ca-bundle.crt
  {{- end }}
  {{- with .Values.server.extraEnv }}
  {{- toYaml . | nindent 2 }}
  {{- end }}
{{- end -}}

{{- define "mainloop.serverMounts" -}}
{{- if .Values.kagentControlAuth.enabled }}
- name: kagent-control-auth
  mountPath: /run/kagent-control-auth
  readOnly: true
- name: kagent-control-ca
  mountPath: /run/kagent-control-ca
  readOnly: true
{{- end }}
{{- with .Values.server.extraVolumeMounts }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/* One listener container. Takes root, name, subcommand, ports, probe port, resources, defaults. */}}
{{- define "mainloop.listener" -}}
{{- $v := .root.Values -}}
- name: {{ .name }}
  image: {{ include "mainloop.backendImage" .root }}
  {{- include "mainloop.pullPolicy" .root | nindent 2 }}
  {{- if $v.server.command }}
  command: {{ toJson $v.server.command }}
  args: [{{ .subcommand }}]
  {{- else }}
  command: [/usr/local/bin/mainloop, {{ .subcommand }}]
  {{- end }}
  {{- with $v.server.workingDir }}
  workingDir: {{ . }}
  {{- end }}
  ports:
    {{- toYaml .ports | nindent 4 }}
  {{- include "mainloop.securityContext" .root | nindent 2 }}
  {{- include "mainloop.serverEnv" .root | nindent 2 }}
  {{- $mounts := trim (include "mainloop.serverMounts" .root) }}
  {{- if or .mounts $mounts }}
  volumeMounts:
    {{- with .mounts }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
    {{- with $mounts }}
    {{- . | nindent 4 }}
    {{- end }}
  {{- end }}
  resources:
    {{- toYaml .resources | nindent 4 }}
  livenessProbe:
    {{- toYaml (mergeOverwrite (deepCopy .liveness) $v.server.livenessProbe) | nindent 4 }}
  readinessProbe:
    {{- toYaml (mergeOverwrite (deepCopy .readiness) $v.server.readinessProbe) | nindent 4 }}
{{- end -}}
