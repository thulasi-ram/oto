#!/usr/bin/env bash
#
# The chart gate. `just helm-check` and the `helm` job in
# .github/workflows/ci.yml both run THIS FILE, so there is one copy of the
# matrix rather than two that drift (README.md: a contributor's green must be
# CI's green).
#
# ⭐ WHY THIS IS NOT JUST `helm lint`. `helm lint` on this chart, at its own
# defaults, exits 0 — while `helm template` on the same inputs exits 1, because
# `oto.validateValues` refuses an install with no database URL. Helm 4.1.1
# demotes a template `fail` to an INFO log line during lint and reports
# "0 chart(s) failed". A gate built on `helm lint` alone would therefore be
# green on a chart that CANNOT RENDER AT ALL, which is the exact shape of
# green-that-means-nothing this repository keeps deleting. `helm lint` is still
# run below and does earn its place — it catches a parse error in any template,
# reached by a flag or not, and a few Kubernetes-specific rules (a Deployment
# whose selector went missing), both with a better message than a render gives.
# It is simply blind to the guard rails, so it cannot BE the gate.
#
# ⭐ AND WHY IT IS NOT JUST `helm template` EITHER. `helm template` proves the Go
# templates execute and emit parseable YAML. It does not know what Kubernetes
# is: a Service declaring `apiVersion: v1beta9`, or a Deployment with no
# `spec.selector`, renders and exits 0. Both were tried against this chart while
# writing this file, and both passed `helm template` and failed `kubeconform`.
# `helm template --validate` would catch them, but it needs a live cluster and
# so cannot run in a per-PR job. `kubeconform` is one static Go binary that
# answers offline from published JSON schemas, and it is the whole reason
# rendering here means "valid Kubernetes" rather than "valid YAML".
#
# ⚠️ A MISSING TOOL IS FATAL, NEVER SKIPPED. The same argument the justfile
# makes about golangci-lint: a gate that quietly passes when its checker is
# absent is worse than no gate, because it reports success.

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
chart="$here/oto"

# A DSN that is merely well-formed. Nothing connects to it; it exists because
# almost every render needs the first guard rail satisfied before it can say
# anything about the rest of the chart.
dsn='postgres://oto:pw@postgres.db.svc:5432/oto?sslmode=require'

# Chart.yaml promises `kubeVersion: ">=1.23.0-0"`. Validating at the FLOOR is
# what makes that promise mechanical — a field that only exists from 1.25 would
# pass at head and break the oldest cluster the chart claims. Validating at a
# current version as well catches the opposite: an API removed since.
k8s_floor=1.23.0
k8s_head=1.31.0

fails=0
pass() { printf '  ✓ %s\n' "$1"; }
bad() { printf '  ✗ %s\n' "$1" >&2; fails=$((fails + 1)); }

# ⭐ RESOLVED BY ABSOLUTE PATH BEFORE PATH IS CONSULTED. `just lint` invokes
# golangci-lint through "$(go env GOPATH)/bin/..." and says why: a wrapper on
# PATH answered for a binary that was not installed, printed "No issues found"
# and exited 0. `just setup` installs BOTH of this gate's tools into that same
# directory — kubeconform with `go install`, helm from its release tarball — so
# the same argument covers both, and looking there FIRST also means the pinned
# copy wins over whatever version a package manager left on PATH. PATH is the
# fallback rather than the first answer because .github/workflows/ci.yml
# installs both into /usr/local/bin instead.
tool() {
  local name="$1" hint="$2" gobin=""
  if command -v go >/dev/null 2>&1; then gobin="$(go env GOPATH)/bin"; fi
  if [ -n "$gobin" ] && [ -x "$gobin/$name" ]; then
    printf '%s\n' "$gobin/$name"
    return 0
  fi
  if command -v "$name" >/dev/null 2>&1; then
    command -v "$name"
    return 0
  fi
  printf '%s is not installed, and the chart gate cannot run without it.\n' "$name" >&2
  printf '  run:  just setup\n' >&2
  printf '  or:   %s\n' "$hint" >&2
  exit 1
}

helm="$(tool helm 'https://helm.sh/docs/intro/install/')"
kubeconform="$(tool kubeconform 'go install github.com/yannh/kubeconform/cmd/kubeconform@v0.7.0')"

# ---------------------------------------------------------------- rendering

# render <name> -- <helm args...>
#
# Renders, requires success, then validates the manifests as Kubernetes at both
# ends of the supported version range. Leaves the manifests in $rendered for the
# caller to assert over.
rendered=""
render() {
  local name="$1"
  shift 2 # the name and the literal `--`
  local out rc
  out="$("$helm" template oto "$chart" "$@" 2>&1)" && rc=0 || rc=$?
  if [ "$rc" -ne 0 ]; then
    bad "$name: helm template exited $rc"
    printf '%s\n' "$out" | sed 's/^/      /' >&2
    rendered=""
    return 0
  fi
  rendered="$out"
  local kv
  for kv in "$k8s_floor" "$k8s_head"; do
    local verdict
    if ! verdict="$(printf '%s\n' "$out" | "$kubeconform" -strict -summary \
      -kubernetes-version "$kv" - 2>&1)"; then
      bad "$name: not valid Kubernetes at $kv"
      printf '%s\n' "$verdict" | sed 's/^/      /' >&2
      return 0
    fi
  done
  pass "$name renders and validates as Kubernetes ($k8s_floor, $k8s_head)"
}

# objects <name> -- <template.yaml>:<Kind>...
#
# The render contains EXACTLY these documents — one entry per rendered object,
# repeated when a template emits more than one of a kind. Absence is asserted by
# the same list: a template not named here must contribute nothing.
#
# ⭐ IT COUNTS OBJECTS, NOT SOURCE MARKERS, AND THAT IS THE WHOLE POINT. The
# obvious version of this assertion greps for `# Source: oto/templates/x.yaml`
# and reads a hit as "x rendered". A source marker only proves the FILE emitted
# something, and helm writes ONE marker per file however many documents follow
# it. job-migrate.yaml emits TWO: the migration hook's Secret, behind
# `if not .Values.existingSecret`, and the Job, which is behind no such thing.
# Its marker is therefore present under EVERY value of `existingSecret` — so the
# check that existed to prove `--set existingSecret=…` renders no Secret of the
# chart's own was answered by a filename that could not tell the Secret from the
# Job, and stayed green while an inverted condition put the operator's DSN into a
# Helm-managed Secret beside their sealed one. Naming the kind of every document
# makes the SET the assertion: a second object appearing in any template, or the
# wrong one of two disappearing, has to fail here.
objects() {
  local name="$1"
  shift 2 # the name and the literal `--`
  [ -n "$rendered" ] || return 0
  local want got
  want="$(printf '%s\n' "$@" | LC_ALL=C sort)"
  got="$(printf '%s\n' "$rendered" | awk '
    /^# Source: / { src = $3; sub(/^oto\/templates\//, "", src); next }
    /^kind: /     { print src ":" $2 }
  ' | LC_ALL=C sort)"
  if [ "$want" = "$got" ]; then
    pass "$name renders exactly $# object(s)"
    return 0
  fi
  bad "$name rendered the wrong set of objects (< expected, > actual)"
  diff <(printf '%s\n' "$want") <(printf '%s\n' "$got") | sed 's/^/      /' >&2 || true
}

# ------------------------------------------------------------- guard rails

# guard <expected message fragment> -- <helm args...>
#
# The inverse assertion: this render MUST fail, and MUST fail with the sentence
# the chart author wrote. An `.Values` path renamed out from under a guard, an
# inverted condition, or a `{{- if -}}` that swallowed its own body all leave
# the guard silently unreachable — the defect class tools/lintreach exists for,
# one layer out — and only asserting the message catches it.
guard() {
  local want="$1"
  shift 2 # the message and the literal `--`
  local out rc
  out="$("$helm" template oto "$chart" "$@" 2>&1)" && rc=0 || rc=$?
  if [ "$rc" -eq 0 ]; then
    bad "guard did not fire: $want"
  elif printf '%s\n' "$out" | grep -qF "$want"; then
    pass "guard fires: $want"
  else
    bad "guard failed with the wrong message (wanted: $want)"
    printf '%s\n' "$out" | sed 's/^/      /' >&2
  fi
}

# ------------------------------------------------------------------ the run

echo '==> helm lint'
# Given the DSN, because at the chart's own defaults the first guard rail is
# supposed to fire — see the note at the top about what lint does with that.
"$helm" lint "$chart" --set-string secrets.databaseUrl="$dsn"

echo '==> renders'

# 1. The happy path: a database URL and nothing else, which is the whole of
#    SPEC acceptance criterion 31. The chart owns the Secret here, and the
#    pre-install migrate hook gets its own short-lived one (_helpers.tpl) — TWO
#    Secrets, which is exactly what an assertion phrased in filenames could not
#    say. ingress.yaml, hpa.yaml and job-bootstrap.yaml are absent from the list
#    and that is the assertion that they render nothing at the defaults.
render 'defaults + databaseUrl' -- --set-string secrets.databaseUrl="$dsn"
objects 'defaults + databaseUrl' -- \
  configmap.yaml:ConfigMap \
  secret.yaml:Secret \
  service.yaml:Service \
  serviceaccount.yaml:ServiceAccount \
  deployment-api.yaml:Deployment \
  deployment-worker.yaml:Deployment \
  job-migrate.yaml:Secret \
  job-migrate.yaml:Job \
  pdb.yaml:PodDisruptionBudget

# 2. Everything the defaults switch off. `ingress.enabled`, `api.autoscaling.
#    enabled` and `bootstrap.enabled` are all false in values.yaml, so three
#    templates are INVISIBLE to any single render — a syntax error in any of
#    them would survive a gate that only ran the happy path.
render 'ingress + autoscaling + bootstrap' -- \
  --set-string secrets.databaseUrl="$dsn" \
  --set ingress.enabled=true \
  --set api.autoscaling.enabled=true \
  --set bootstrap.enabled=true \
  --set bootstrap.orgSlug=acme \
  --set bootstrap.email=a@b.c \
  --set-string secrets.bootstrapPassword=hunter2
objects 'ingress + autoscaling + bootstrap' -- \
  configmap.yaml:ConfigMap \
  secret.yaml:Secret \
  service.yaml:Service \
  serviceaccount.yaml:ServiceAccount \
  deployment-api.yaml:Deployment \
  deployment-worker.yaml:Deployment \
  hpa.yaml:HorizontalPodAutoscaler \
  ingress.yaml:Ingress \
  job-migrate.yaml:Secret \
  job-migrate.yaml:Job \
  job-bootstrap.yaml:Job \
  pdb.yaml:PodDisruptionBudget

# 3. `existingSecret` must render NO Secret AT ALL — the promise values.yaml
#    makes to anyone using sealed-secrets or External Secrets, and a chart that
#    renders one anyway overwrites the operator's. NOT "no secret.yaml": the
#    migration hook's Secret lives in job-migrate.yaml and is the one that
#    actually carries the DSN, so it is its ABSENCE from this list, beside a
#    job-migrate.yaml that still contributes its Job, that carries the promise.
render 'existingSecret' -- --set existingSecret=oto-secrets
objects 'existingSecret' -- \
  configmap.yaml:ConfigMap \
  service.yaml:Service \
  serviceaccount.yaml:ServiceAccount \
  deployment-api.yaml:Deployment \
  deployment-worker.yaml:Deployment \
  job-migrate.yaml:Job \
  pdb.yaml:PodDisruptionBudget

# 4. Slack over HTTP, the mode that needs a signing secret. Rendering it proves
#    the guard below rejects the MISSING secret rather than the mode itself, and
#    the object set proves the mode adds no object of its own: the signing secret
#    is a key in the Secret the chart already owns, not a second one.
render 'slack http' -- \
  --set-string secrets.databaseUrl="$dsn" \
  --set config.slack.enabled=true \
  --set config.slack.mode=http \
  --set-string secrets.slackSigningSecret=shh
objects 'slack http' -- \
  configmap.yaml:ConfigMap \
  secret.yaml:Secret \
  service.yaml:Service \
  serviceaccount.yaml:ServiceAccount \
  deployment-api.yaml:Deployment \
  deployment-worker.yaml:Deployment \
  job-migrate.yaml:Secret \
  job-migrate.yaml:Job \
  pdb.yaml:PodDisruptionBudget

echo '==> guard rails (_helpers.tpl "oto.validateValues")'

# 1. No credentials at all — which is also what the chart's own defaults are,
#    so this is the render `helm lint` claims is fine.
guard 'set either secrets.databaseUrl or existingSecret' --

# 2. A comma anywhere in the DSN. koanf's env provider splits on it, so the
#    value never reaches pgx. `--set-string` with an escaped comma, because
#    `--set` would split the argument itself and never reach the guard.
guard 'secrets.databaseUrl contains a comma' -- \
  --set-string 'secrets.databaseUrl=postgres://oto:p\,w@postgres.db.svc:5432/oto'

# 3. Slack in http mode with no signing secret: the endpoint would authenticate
#    nothing.
guard "config.slack.mode is 'http' and secrets.slackSigningSecret is empty" -- \
  --set-string secrets.databaseUrl="$dsn" \
  --set config.slack.enabled=true \
  --set config.slack.mode=http

# 4. An environment name the binary does not accept. `production` is the
#    plausible typo for `prod`.
guard 'config.env must be one of dev, staging, prod' -- \
  --set-string secrets.databaseUrl="$dsn" \
  --set config.env=production

# 5. The connection split that starves the general pool. Both arms of
#    `max(share%, min_conns)` are exercised, because the guard does integer
#    arithmetic in Go templates and one arm can rot without the other.
guard 'leave no connections for the general pool' -- \
  --set-string secrets.databaseUrl="$dsn" \
  --set config.db.ingest_share_percent=100
guard 'leave no connections for the general pool' -- \
  --set-string secrets.databaseUrl="$dsn" \
  --set config.db.ingest_min_conns=20

# 6, 7, 8. The three bootstrap preconditions, each reached only once the
#    previous one is satisfied — so they must be asserted in order, and a guard
#    that stopped firing would otherwise hide behind the one before it.
guard 'bootstrap.enabled is true but bootstrap.orgSlug is empty' -- \
  --set-string secrets.databaseUrl="$dsn" \
  --set bootstrap.enabled=true

guard 'bootstrap.enabled is true but bootstrap.email is empty' -- \
  --set-string secrets.databaseUrl="$dsn" \
  --set bootstrap.enabled=true \
  --set bootstrap.orgSlug=acme

guard 'bootstrap.enabled is true but no password is available' -- \
  --set-string secrets.databaseUrl="$dsn" \
  --set bootstrap.enabled=true \
  --set bootstrap.orgSlug=acme \
  --set bootstrap.email=a@b.c

echo
if [ "$fails" -ne 0 ]; then
  printf '✗ chart gate: %d failure(s)\n' "$fails" >&2
  exit 1
fi
echo '✓ chart gate clean'
