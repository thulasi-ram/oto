{{/*
Standard name helpers.
*/}}

{{- define "oto.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "oto.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "oto.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Labels. `oto.labels` goes on every object; `oto.selectorLabels` is the immutable
subset a Deployment selector may use. The component is passed in by the caller:
  {{ include "oto.selectorLabels" (dict "ctx" . "component" "api") }}
*/}}

{{- define "oto.labels" -}}
helm.sh/chart: {{ include "oto.chart" .ctx }}
{{ include "oto.selectorLabels" . }}
app.kubernetes.io/version: {{ .ctx.Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .ctx.Release.Service }}
app.kubernetes.io/part-of: oto
{{- with .ctx.Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{- define "oto.selectorLabels" -}}
app.kubernetes.io/name: {{ include "oto.name" .ctx }}
app.kubernetes.io/instance: {{ .ctx.Release.Name }}
{{- with .component }}
app.kubernetes.io/component: {{ . }}
{{- end }}
{{- end -}}

{{- define "oto.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "oto.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
The image reference. `image.tag` falls back to the chart's appVersion so a chart
release and an application release move together unless the operator pins.
*/}}
{{- define "oto.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}

{{/*
The Secret the pods read. `existingSecret` wins, and when it is set this chart
renders no Secret of its own — the whole point being that real deployments keep
secret material in whatever store the cluster already trusts.
*/}}
{{- define "oto.secretName" -}}
{{- default (include "oto.fullname" .) .Values.existingSecret -}}
{{- end -}}

{{- define "oto.configMapName" -}}
{{- printf "%s-config" (include "oto.fullname" .) -}}
{{- end -}}

{{/*
The Secret the MIGRATION HOOK reads.

⭐ IT IS A SEPARATE, SHORT-LIVED SECRET WHEN THIS CHART OWNS THE CREDENTIALS, and
that is not an accident. A pre-install hook runs BEFORE the release's ordinary
resources exist, so the main Secret is not there yet. The two ways out are to
annotate the main Secret as a hook — which orphans it on `helm uninstall`,
because Helm does not track hook resources in the release — or to give the hook
its own Secret carrying only the one key it needs and delete it when the hook
finishes. This chart does the second: `<fullname>-migrate` holds OTO_DB_URL,
lives for the length of the hook, and the main Secret stays an ordinary
release-tracked object that `helm uninstall` removes.

When `existingSecret` is set there is no problem to solve: that Secret already
exists before the install, so the hook reads it directly.
*/}}
{{- define "oto.migrationSecretName" -}}
{{- if .Values.existingSecret -}}
{{- .Values.existingSecret -}}
{{- else -}}
{{- printf "%s-migrate" (include "oto.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/*
`envFrom` for every oto pod: the non-secret ConfigMap, then anything the operator
added. The container's own `env:` wins over all of it.
*/}}
{{- define "oto.envFrom" -}}
- configMapRef:
    name: {{ include "oto.configMapName" . }}
{{- with .Values.extraEnvFrom }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/*
The secret-backed environment, projected KEY BY KEY rather than with an
`envFrom: secretRef`.

⭐ THAT IS WHY `existingSecretKeys` CAN RENAME ANYTHING. An `envFrom` over the
whole Secret makes every key the variable of the same name, so a secret store
that already calls the DSN `DATABASE_URL` would hand oto a variable it does not
read and the pod would boot against config.Default()'s localhost — a wrong
database rather than an error. Naming each key explicitly means the chart adapts
to the operator's Secret instead of demanding the operator adapt to the chart.

⛔ EVERYTHING EXCEPT THE DSN IS `optional: true`, because everything except the
DSN is genuinely optional to the binary. A missing OTO_SECURITY_SECRET_KEY makes
oto boot without a keyring and say so; a missing DSN is not a state oto can run
in, so the pod stays Pending with a message naming the key it wanted.
*/}}
{{- define "oto.secretEnv" -}}
- name: OTO_DB_URL
  valueFrom:
    secretKeyRef:
      name: {{ include "oto.secretName" . }}
      key: {{ .Values.existingSecretKeys.databaseUrl }}
- name: OTO_SECURITY_SECRET_KEY
  valueFrom:
    secretKeyRef:
      name: {{ include "oto.secretName" . }}
      key: {{ .Values.existingSecretKeys.secretKey }}
      optional: true
- name: OTO_SLACK_APP_TOKEN
  valueFrom:
    secretKeyRef:
      name: {{ include "oto.secretName" . }}
      key: {{ .Values.existingSecretKeys.slackAppToken }}
      optional: true
- name: OTO_SLACK_SIGNING_SECRET
  valueFrom:
    secretKeyRef:
      name: {{ include "oto.secretName" . }}
      key: {{ .Values.existingSecretKeys.slackSigningSecret }}
      optional: true
{{- end -}}

{{/*
Guard rails that fail `helm install` with a sentence instead of failing the pod
with a CrashLoopBackOff twenty seconds later.
*/}}
{{- define "oto.validateValues" -}}
{{- if and (not .Values.existingSecret) (not .Values.secrets.databaseUrl) -}}
{{- fail "oto: set either secrets.databaseUrl or existingSecret. Postgres is EXTERNAL to this chart (ADR 0014): there is no bundled database and oto cannot start without a URL." -}}
{{- end -}}
{{- if contains "," .Values.secrets.databaseUrl -}}
{{- fail "oto: secrets.databaseUrl contains a comma. oto's config loader splits any environment value containing a comma into a list, so a comma in the password or a multi-host DSN never reaches pgx. Percent-encode it or use existingSecret." -}}
{{- end -}}
{{- if and .Values.config.slack.enabled (eq .Values.config.slack.mode "http") (not .Values.existingSecret) (not .Values.secrets.slackSigningSecret) -}}
{{- fail "oto: config.slack.mode is 'http' and secrets.slackSigningSecret is empty. oto refuses to boot in that state, because the signing secret is the only thing authenticating the Slack interactions endpoint." -}}
{{- end -}}
{{- if not (has .Values.config.env (list "dev" "staging" "prod")) -}}
{{- fail "oto: config.env must be one of dev, staging, prod." -}}
{{- end -}}
{{- $ingest := div (mul (int .Values.config.db.max_conns) (int .Values.config.db.ingest_share_percent)) 100 -}}
{{- $ingest = int (max $ingest (int .Values.config.db.ingest_min_conns)) -}}
{{- if ge $ingest (int .Values.config.db.max_conns) -}}
{{- fail "oto: config.db.ingest_share_percent and ingest_min_conns leave no connections for the general pool. oto opens TWO pools over one database (SPEC §G.10) and validates this at boot." -}}
{{- end -}}
{{- if .Values.bootstrap.enabled -}}
{{- if not .Values.bootstrap.orgSlug -}}
{{- fail "oto: bootstrap.enabled is true but bootstrap.orgSlug is empty." -}}
{{- end -}}
{{- if not .Values.bootstrap.email -}}
{{- fail "oto: bootstrap.enabled is true but bootstrap.email is empty." -}}
{{- end -}}
{{- if and (not .Values.existingSecret) (not .Values.secrets.bootstrapPassword) -}}
{{- fail "oto: bootstrap.enabled is true but no password is available. Set secrets.bootstrapPassword, or put OTO_BOOTSTRAP_PASSWORD in existingSecret." -}}
{{- end -}}
{{- end -}}
{{- end -}}
