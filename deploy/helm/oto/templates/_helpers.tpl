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
The hook annotations for one of the two sequenced Jobs, spelled by whatever is
applying the manifests.

Called as (dict "ctx" . "job" "migrate"|"bootstrap"|"migrate-secret").

⭐ IT EXISTS BECAUSE `helm.sh/hook` IS NOT A PORTABLE ORDERING PRIMITIVE. helm
runs a pre-install hook before it applies the release's ordinary resources; Argo
CD reads the same annotation, maps it onto PreSync, and runs it before the
release's ordinary resources have been APPLIED AT ALL — so on a first install the
migrate Job starts before the ExternalSecret that was going to materialise its
database URL exists, and fails with "secret not found". A sync wave is a
different primitive: Argo gates a wave on the previous wave being healthy, which
is the guarantee the Job actually needs.

⛔ AN ARGO HOOK IS `Sync` AND NOT `PreSync`, DELIBERATELY. PreSync is the same
trap by another name: it runs before wave 0, so nothing a consumer put there —
their SecretStore, their ExternalSecret — is ready. A `Sync` hook is ordered by
its wave alongside everything else, which is the whole point.

⚠️ `before-hook-creation` IS NOT OPTIONAL IN EITHER SPELLING. A Job's spec is
immutable, so re-applying one with a changed template is rejected outright unless
the previous object is deleted first. helm and Argo each have their own name for
that policy and neither reads the other's.

⛔ THE MIGRATION SECRET IS A HOOK UNDER helm AND AN ORDINARY WAVE-0 RESOURCE UNDER
argocd. Under helm it has to be a hook, because a pre-install hook cannot read a
resource helm has not applied yet. Under Argo there is no such phase: the migrate
Job's wave is gated on wave 0 being healthy, so the Secret is simply there.
Annotating it as a hook would instead have Argo delete it on `hook-succeeded` and
re-create it every sync, for no gain.

⚠️ SO THAT COPY OF THE DSN OUTLIVES THE SYNC, WHERE THE helm SPELLING DELETES IT.
Not a new exposure: it renders only when `existingSecret` is unset, and in that
configuration `secret.yaml` already holds the same URL as an ordinary
release-tracked object. A GitOps install with an ExternalSecret sets
`existingSecret` and renders neither.

⛔ NO COMMENTS INSIDE THE `define` BELOW. A `{{/* */}}` in a branch body emits its
own newlines, and they land in the rendered manifest as blank lines inside an
`annotations:` block. Valid YAML, passes kubeconform, and visible to every
operator who reads what this chart produced.
*/}}
{{- define "oto.hookAnnotations" -}}
{{- $ctx := .ctx -}}
{{- $job := .job -}}
{{- $provider := $ctx.Values.hooks.provider -}}
{{- if eq $provider "helm" -}}
{{- if eq $job "migrate-secret" }}
helm.sh/hook: pre-install,pre-upgrade
helm.sh/hook-weight: "-10"
helm.sh/hook-delete-policy: before-hook-creation,hook-succeeded
{{- else if eq $job "migrate" }}
helm.sh/hook: pre-install,pre-upgrade
helm.sh/hook-weight: "-5"
helm.sh/hook-delete-policy: before-hook-creation,hook-succeeded
{{- else if eq $job "bootstrap" }}
helm.sh/hook: post-install
helm.sh/hook-weight: "5"
helm.sh/hook-delete-policy: before-hook-creation
{{- end }}
{{- else if eq $provider "argocd" -}}
{{- if eq $job "migrate-secret" }}
argocd.argoproj.io/sync-wave: "0"
{{- else if eq $job "migrate" }}
argocd.argoproj.io/hook: Sync
argocd.argoproj.io/hook-delete-policy: BeforeHookCreation
argocd.argoproj.io/sync-wave: {{ $ctx.Values.hooks.waves.migrate | quote }}
{{- else if eq $job "bootstrap" }}
argocd.argoproj.io/hook: Sync
argocd.argoproj.io/hook-delete-policy: BeforeHookCreation
argocd.argoproj.io/sync-wave: {{ $ctx.Values.hooks.waves.bootstrap | quote }}
{{- end }}
{{- end -}}
{{- end -}}

{{/*
The ordering annotation for the WORKLOADS the migrate Job exists to unblock.

⭐ WITHOUT IT `hooks.provider: argocd` ORDERS THE JOBS AND ABANDONS THE
DEPLOYMENTS. An unannotated resource lands in wave 0, so the api and worker
Deployments would be applied a full wave BEFORE the migration they depend on: the
api pods come up against a schema that does not match and sit unready (which
/readyz reports honestly), and the worker exits and CrashLoopBackOffs until the
Job lands. It self-heals in minutes and it looks exactly like a broken install
while it does — and under helm none of this arises, because there the Deployments
are ordinary resources and every hook phase precedes them.

⚠️ THE SAME WAVE AS bootstrap BY DEFAULT, AND THAT IS NOT AN OVERSIGHT. Neither
needs the other: `oto bootstrap` writes rows through Postgres and never calls the
API, and the API serves before an org exists. Both need only the migration, so
both belong in the wave after it.

⛔ IT RENDERS NOTHING UNDER helm OR none. A `sync-wave` on a helm-installed
Deployment is a no-op annotation that implies an ordering helm is not performing,
and the next person to read it would believe it.
*/}}
{{- define "oto.workloadAnnotations" -}}
{{- if eq .Values.hooks.provider "argocd" }}
argocd.argoproj.io/sync-wave: {{ .Values.hooks.waves.workloads | quote }}
{{- end }}
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
{{- if not (has .Values.hooks.provider (list "helm" "argocd" "none")) -}}
{{- fail "oto: hooks.provider must be one of helm, argocd, none. It decides who sequences the migrate and bootstrap Jobs; a value nothing recognises would render both with NO ordering annotation at all, which looks like it worked until a worker boots against an unmigrated database." -}}
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
