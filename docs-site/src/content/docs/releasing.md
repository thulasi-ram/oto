---
title: Releasing oto
---
A release is a git tag. Everything published is derived from it, and nothing
else triggers the pipeline: `.github/workflows/release.yml` has no
`workflow_dispatch`, deliberately, because dispatched from a branch there is no
version for `docker/metadata-action` to derive and the run would publish an
artifact with no name anyone could pin.

There are two commands and one of them is irreversible.

```bash
just release v0.1.0     # local: eight checks, then an annotated tag
git push origin v0.1.0  # publishes — see "What a push publishes"
```

`just release` never pushes. Creating a tag is local and free to undo with
`git tag -d`; pushing one puts an immutable image in a public registry under a
name that is never re-cut. The two are separate so the irreversible half is
typed on purpose.

## Before the tag: what the version number has to agree with

`deploy/helm/oto/Chart.yaml` carries two versions and they are not the same
fact.

| Field | What it means | Must it match the tag? |
|---|---|---|
| `appVersion` | The oto build this chart deploys by default | **Yes**, without the leading `v` |
| `version` | The chart's own version, bumped when templates or defaults change | No — it may move on its own |

`appVersion` is load-bearing rather than descriptive: `_helpers.tpl` falls back
to `.Chart.AppVersion` verbatim when `image.tag` is unset, so it *is* the image
tag a default `helm install` resolves to. That is why the pipeline publishes the
bare `0.1.0` form alongside `v0.1.0`, and why `just release` refuses a tag whose
`appVersion` disagrees: the two drifting apart is an ImagePullBackOff on a fresh
install, which is the failure the whole workflow exists to end.

So bumping the version is an ordinary commit on `main`, reviewed and merged like
any other, and the tag comes afterwards:

```bash
# edit deploy/helm/oto/Chart.yaml — appVersion, and `version` if the chart moved
git commit -am 'chore(helm): the chart deploys 0.2.0 by default'
git push origin main       # let ci run
just release v0.2.0        # refuses until ci is green on this exact commit
```

## What `just release` checks, and why each one is there

Each check is a failure that is cheap here and expensive after a tag exists. The
pipeline cannot make any of them, because by the time it runs, the name is
already taken.

1. **The tag matches the trigger glob** (`v[0-9]+.[0-9]+.[0-9]+*`). A tag that
   does not match does not fail the pipeline — it publishes *nothing*, silently,
   and the first symptom is an operator pulling a tag that was never there. The
   trailing `*` admits `v0.1.0-rc.1` without a second workflow.
2. **The working tree is clean.** A tag names a commit. Tagging over unstaged
   changes publishes an image built from a tree nobody can check out.
3. **You are on `main`.**
4. **`HEAD` and `origin/main` agree.** Releasing from a commit others do not have
   ships code that never saw a pull request.
5. **The tag does not already exist**, locally or on the remote.
6. **`Chart.yaml`'s `appVersion` equals the tag without its `v`.** See above. A
   mismatched chart `version` is a warning, not a refusal.
7. **`ci` concluded `success` on this exact commit**, via `gh`. This one is not
   optional and it is not belt-and-braces: **the release pipeline runs no tests
   at all.** That is the intended bargain — `ci` triggers on pushes to `main` and
   on pull requests, so re-running eight jobs here would double the wall clock to
   re-answer a question already answered. The bargain holds only while something
   confirms the answer was green, and nothing downstream of the tag does.
8. **Both architectures cross-compile**, by building the real Dockerfile for
   `linux/amd64` and `linux/arm64`. A broken build fails on a laptop in two
   minutes instead of after a tag has taken a name that cannot be reused.

## What a push publishes

| Tag | From | Notes |
|---|---|---|
| `0.1.0` | `type=semver,pattern={{version}}` | The bare form. **This is what the chart resolves to.** |
| `0.1` | `type=semver,pattern={{major}}.{{minor}}` | The moving minor line. Withheld from a prerelease automatically. |
| `v0.1.0` | `type=ref,event=tag` | So the image tag and the git tag can be named with the same string. |
| `sha-<commit>` | `type=sha,format=long` | Forensics: commit → image, without a release note. |

All under `ghcr.io/thulasi-ram/oto`, for `linux/amd64` and `linux/arm64`.

**There is no `latest`, on purpose.** `values.yaml` argues in the chart's own
voice that a floating tag makes "which build is this" unanswerable, and
`Chart.yaml` marks the chart `artifacthub.io/prerelease: "true"`. Publishing a
`latest` nobody should use, while telling operators not to use one, is a
contradiction. It can be added at 1.0, when it means something.

The pipeline also writes a signed build-provenance attestation to the registry,
so "which commit produced this digest" is answerable with
`gh attestation verify` rather than by trusting a release note.

### And the chart, as an OCI artifact

```
oci://ghcr.io/thulasi-ram/charts/oto  --version 0.1.1
```

`deploy/helm/oto` used to be reachable only by cloning this repository, which
made every consumer vendor a copy — and a vendored chart silently stops matching
the image its `appVersion` names. A GitOps consumer cannot read a chart from a
git path at all: `kustomize`'s `helmCharts` accepts an HTTP Helm repo or an
`oci://` reference and nothing else, and Argo CD renders with
`kustomize build --enable-helm`.

The `chart` job runs `needs: image`, so a chart is never published beside an
image that failed to build — that combination installs cleanly and then
ImagePullBackOffs every pod. `--version` and `--app-version` are both derived
from the tag, so the chart version, its `appVersion` and the image tag are the
same string by construction. It packages, **renders the packaged tarball and
asserts the image reference before pushing**, pushes, then pulls the published
artifact back and renders that too. The pre-push assertion is the one that
matters: `helm package` rewrites `appVersion`, so the tarball is bytes `ci`'s
chart gate never saw, and finding a wrong image tag after the push means finding
it once the reference is already public and pinnable.

It deliberately does **not** re-run `deploy/helm/check.sh`. The lint, render and
kubeconform matrix belongs to `ci`, by the same bargain as the test suite, and
packaging can change exactly one fact — which image tag the chart resolves to.

### What it asserts before it finishes

Two checks run against the image that was actually published, pulled back by
digest:

- **It knows which build it is.** `oto version` must print
  `oto <tag> (commit <sha>, built …)`. Every artifact this repository produced
  before the pipeline existed answered `oto dev`, because nothing passed the
  three linker stamps the Dockerfile has always declared — making the one
  question `GET /api/v1/version` exists to answer unanswerable.
- **The manifest lists both architectures.** A single-architecture manifest is an
  ImagePullBackOff on half the clusters it was promised to.

## Package visibility

**Checked on the first real run (`v0.1.0`) and it needed no intervention.** The
package inherited this repository's public visibility, so the four tags were
anonymously pullable the moment the workflow finished — verified with a token
from `ghcr.io/token` carrying no credentials, and by `docker run` on a machine
with no `ghcr.io` entry in its Docker config.

This is worth stating because the expectation recorded during the build was the
opposite — that a new GHCR package is private until somebody flips it — and a
release note that sends a maintainer to change a setting that is already correct
is its own small trap.

It is still the thing to check first if a pull fails for someone outside the org,
because nothing in the workflow can fix it: the built-in `GITHUB_TOKEN` pushes to
a package under the repository's own owner and cannot change its visibility.

*GitHub → the `oto` package → Package settings → Change visibility.*

### The chart is a second package, and it has to be checked once too

`charts/oto` is its own GHCR package with its own visibility, so the `oto`
package being public says nothing about it. Two things make it likely to inherit
this repository's visibility the same way: the repository is public, and helm
turns `Chart.yaml`'s `sources[0]` into the `org.opencontainers.image.source`
annotation GHCR links a package to a repository by.

**Likely is not verified, and the failure mode is silent.** The pipeline's own
pull-back step authenticates, so it cannot tell a public package from a private
one. Confirm it anonymously after the first chart release:

```bash
# with no credentials for ghcr.io in this shell
helm registry logout ghcr.io 2>/dev/null || true
helm pull oci://ghcr.io/thulasi-ram/charts/oto --version 0.1.1
```

`unauthorized` means the package needs the one-time flip at *GitHub → the
`charts/oto` package → Package settings → Change visibility*. It matters more
here than it does for the image: a Helm consumer can be handed a registry login
and `imagePullSecrets`, but **`kustomize`'s `helmCharts` has no credential path
at all**, so an Argo CD repo-server pulling a private chart gets `unauthorized`
and the `Application` never renders, with nothing useful in the error.

For a genuinely private deployment the answer is `imagePullSecrets`, which the
chart already projects into all four pod specs.

## Verifying a release

What `v0.1.0` actually published, as the shape to expect:

```
ghcr.io/thulasi-ram/oto  0.1.0  0.1  v0.1.0  sha-267d5e12…  (linux/amd64, linux/arm64)
oto v0.1.0 (commit 267d5e12c87f4d12eee8deaac8f73d2d7db84619, built 2026-08-26T10:40:39.670Z, go1.26.7)
helm template … → image: ghcr.io/thulasi-ram/oto:0.1.0
```

That last line is the whole point of the appVersion check: the chart's default,
with no `image.tag` set, resolves to a tag that exists.

```bash
docker pull ghcr.io/thulasi-ram/oto:0.1.0
docker run --rm ghcr.io/thulasi-ram/oto:0.1.0 version
docker buildx imagetools inspect ghcr.io/thulasi-ram/oto:0.1.0
gh attestation verify oci://ghcr.io/thulasi-ram/oto:0.1.0 --repo thulasi-ram/oto

helm template oto deploy/helm/oto --set secrets.databaseUrl=postgres://… \
  | grep 'image:'   # resolves to appVersion when image.tag is unset

# the chart, from the first release that publishes one — v0.1.1 onward, since
# v0.1.0 shipped an image and nothing else
helm pull oci://ghcr.io/thulasi-ram/charts/oto --version 0.1.1
helm show chart oci://ghcr.io/thulasi-ram/charts/oto --version 0.1.1
```

`just release-watch v0.1.0` follows the run and exits non-zero if it failed.

## When a release goes wrong

**A tag is never re-cut.** The concurrency group is deliberately not
`cancel-in-progress`, because cancelling a half-finished multi-architecture push
can leave a manifest list pointing at blobs that never finished uploading — and
there is no later run on the same ref to correct it.

So the answer is always forward. If the pipeline failed before publishing, delete
the tag and cut it again once the cause is fixed:

```bash
git push origin :refs/tags/v0.1.0
git tag -d v0.1.0
```

If it published something wrong, cut the next patch version. Do not delete a tag
whose image somebody may already have pulled: the digest stays valid in their
cluster either way, and removing the tag only makes it harder to work out what
they are running.

## What this process does not do yet

- **No GitHub Release, and no release notes.** The tag is the whole artifact
  today. A `softprops/action-gh-release` step and a `CHANGELOG.md` are the
  obvious next move, and are deliberately absent rather than forgotten — nothing
  currently generates notes worth publishing.
- **No chart publishing.** `deploy/helm/oto` is installed from a checkout. An OCI
  chart push to the same registry is a natural companion to the image, and would
  make `appVersion` and the chart move together in one run.
