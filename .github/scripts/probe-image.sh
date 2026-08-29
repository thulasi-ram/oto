#!/usr/bin/env bash
# probe-image.sh <image-ref>
#
# Boots a published oto image and asserts what it actually SERVES over HTTP.
#
# ⭐ IT EXISTS BECAUSE 0.1.0 THROUGH 0.1.2 SHIPPED NO WEB UI AND NOTHING NOTICED.
# `web/dist` is a build output that is not committed, nothing embedded it, and
# the image answered `/` with `404 page not found` for three releases while
# README told operators to create a source in the UI. Every gate was green
# throughout, because every gate asked about the source tree. This one asks the
# artifact.
#
# ⛔ IT IS A DIFFERENT QUESTION FROM `oto version`'s `ui: embedded (N files)`.
# That proves bytes are inside the binary; this proves the binary hands them to a
# browser and still refuses the API's namespace. Both are cheap and they fail in
# different ways: an embed can be present and unrouted, and a route can be
# present and over-greedy.
#
# ⭐ NO POSTGRES, NO SERVICE CONTAINER, AND THAT IS WHY THIS IS AFFORDABLE. `oto
# api` binds its listener and serves `/`, `/healthz` and the static surface
# without a working database — readiness reports the truth about Postgres, and
# liveness deliberately does not touch it (internal/app/routes.go). So a
# deliberately bogus OTO_DB_URL is enough, and the whole gate is a `docker run`
# and a handful of curls. Credit where due: a downstream consumer found this
# while verifying 0.1.3 by hand, which is also how this file got its table.
#
# ⚠️ THE IMAGE HAS NO SHELL. It is distroless/static, so nothing here may `docker
# exec` or pipe a script into the container; every assertion is made from OUTSIDE
# over the published port. That constraint is the reason the earlier attempt at
# this gate was abandoned as "needs a service container", which was wrong.

set -euo pipefail

ref="${1:-}"
if [ -z "$ref" ]; then
  echo "usage: probe-image.sh <image-ref>" >&2
  exit 2
fi

port="${PROBE_PORT:-18080}"
name="oto-probe-$$"
fails=0

pass() { printf '  ✓ %s\n' "$1"; }
bad() { printf '  ✗ %s\n' "$1" >&2; fails=$((fails + 1)); }

cleanup() {
  # The logs first, and only on failure: a container that refused to serve has
  # already said why, and having to re-run the job to see it is a wasted cycle.
  if [ "$fails" -ne 0 ]; then
    echo '--- container logs ---' >&2
    docker logs "$name" 2>&1 | tail -40 >&2 || true
  fi
  docker rm -f "$name" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "==> booting $ref"
docker run -d --name "$name" -p "$port:8080" \
  -e OTO_DB_URL='postgres://nobody:nothing@127.0.0.1:1/none?sslmode=disable' \
  "$ref" api >/dev/null

base="http://127.0.0.1:$port"

# ⚠️ THE CONTAINER EXITING IS A DISTINCT FAILURE FROM IT BEING SLOW, and a loop
# that only counts down reports the wrong one. `oto api` with an unreachable
# database must STAY UP — if this ever starts exiting, the sentence below is the
# one that says so, rather than a timeout that reads like a slow boot.
deadline=$(( $(date +%s) + 60 ))
until curl -fsS -o /dev/null -m 2 "$base/healthz" 2>/dev/null; do
  if ! docker ps -q --no-trunc --filter "name=^/$name$" | grep -q .; then
    echo "✗ the container exited before serving /healthz." >&2
    echo "  `oto api` is expected to serve without a reachable database:" >&2
    echo "  liveness does not touch Postgres and readiness reports it honestly." >&2
    exit 1
  fi
  if [ "$(date +%s)" -ge "$deadline" ]; then
    echo "✗ /healthz did not answer within 60s" >&2
    exit 1
  fi
  sleep 1
done
pass "the image serves without a database"

# expect <method> <path> <status> <content-type substring, or '!html' to forbid html>
expect() {
  local method="$1" path="$2" want_status="$3" want_type="${4:-}"
  local out status ctype
  out="$(curl -s -o /dev/null -X "$method" -m 10 \
    -w '%{http_code} %{content_type}' "$base$path" || true)"
  status="${out%% *}"
  ctype="${out#* }"

  if [ "$status" != "$want_status" ]; then
    bad "$method $path answered $status, wanted $want_status (content-type: $ctype)"
    return 0
  fi
  case "$want_type" in
    "") ;;
    '!html')
      case "$ctype" in
        *text/html*) bad "$method $path answered HTML, which it must never do" ; return 0 ;;
      esac
      ;;
    *)
      case "$ctype" in
        *"$want_type"*) ;;
        *) bad "$method $path answered content-type $ctype, wanted $want_type"; return 0 ;;
      esac
      ;;
  esac
  pass "$method $path -> $status ${want_type:+($ctype)}"
}

echo '==> the UI is served'
expect GET / 200 text/html
# A client-side route: a bookmark, a pasted link, a refresh. It is not a file on
# disk, and answering 404 here is what makes a SPA work only if you never reload.
expect GET /alerts 200 text/html
expect GET /settings/notifications 200 text/html

echo '==> and the API namespace is never answered by it'
# ⛔ THE ASSERTION THAT CANNOT BE MADE BY LOOKING. An over-greedy fallback shows a
# perfect UI in a browser while programmatic callers get HTML with a 200 — their
# JSON decoder fails somewhere downstream, naming neither oto nor the route.
expect GET /api/v1/definitely-not-a-route 404 '!html'
# `/api/v2/...` matches no mount at all, which is how it once reached the SPA and
# was served index.html with a 200.
expect GET /api/v2/alerts 404 '!html'
expect GET /healthz 200 application/json
expect GET /openapi.json 200 application/json

echo '==> and a missing asset is a missing asset'
# Answered with the shell, a deleted bundle gives the browser HTML with a 200 for
# a <script>: a blank page and a console syntax error, which is exactly what a
# stale cached index.html asks for after a deploy.
expect GET /assets/index-DELETED.js 404 '!html'
# GET and HEAD only: a POST answered 200 index.html tells a client its write
# succeeded.
expect POST /nope 405

echo '==> and the cache headers survive a deploy'
index_cc="$(curl -s -D - -o /dev/null -m 10 "$base/" | tr -d '\r' | awk -F': ' 'tolower($1)=="cache-control"{print $2}')"
case "$index_cc" in
  *no-store*) pass "index.html is $index_cc" ;;
  *) bad "index.html has Cache-Control '$index_cc' and must be no-store: it names hashed bundles the next build deletes" ;;
esac

asset="$(curl -s -m 10 "$base/" | grep -o '/assets/[A-Za-z0-9._-]*\.js' | head -1 || true)"
if [ -z "$asset" ]; then
  bad "index.html references no /assets/*.js — the shell was served but looks empty"
else
  asset_cc="$(curl -s -D - -o /dev/null -m 10 "$base$asset" | tr -d '\r' | awk -F': ' 'tolower($1)=="cache-control"{print $2}')"
  case "$asset_cc" in
    *immutable*) pass "$asset is $asset_cc" ;;
    *) bad "$asset has Cache-Control '$asset_cc' and should be immutable: its name carries a content hash" ;;
  esac
fi

echo
if [ "$fails" -ne 0 ]; then
  printf '✗ image probe: %d failure(s)\n' "$fails" >&2
  exit 1
fi
echo '✓ image probe clean'
