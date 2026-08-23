# Working in `docs-site/`

This directory is a Starlight **mirror** of oto's documentation. It is not wired
to CI and it is not deployed. See `README.md` here for how to run it.

## The source of truth is outside this directory

Every page under `src/content/docs/` except `index.mdx` and the hand-authored
part of `guides/` is **generated**. Editing a generated page is wasted work: the
next sync overwrites it, and the real defect stays in the source file.

Sources live in the repository root (`README.md`, `CONTEXT.md`,
`CONTRIBUTING.md`) and under `docs/` (`concepts.md`, `ORCHESTRATION.md`,
`adr/`, `design/`, `setup/`, `runbooks/`). Fix the source, then re-run the sync.

## The sync contract

`scripts/sync-docs.mjs`, run by `npm run sync-docs` and implied by both
`npm run dev` and `npm run build`:

- derives each page's frontmatter `title` from the source file's first H1 and
  strips that H1 from the body — so a source file with no H1 gets its filename
  as a title, which is almost never what is wanted;
- rewrites relative `*.md` links to the clean routes Starlight serves, using a
  table built from the source list. A link to a file that is not synced is left
  alone rather than guessed at, so it will 404 on the site while still resolving
  in the repository;
- **deletes and rebuilds** `adr/`, `design/`, `setup/` and `runbooks/` under
  `src/content/docs/` on every run. A page in one of those four directories with
  no source under `docs/` is destroyed, not merely refreshed. Everything else is
  written in place.

Adding a new top-level source file means adding it to `SOURCES` in the script
**and**, if it belongs in the Overview group, to the sidebar in
`astro.config.mjs`. Files inside the four mirrored directories need neither:
they are picked up by the directory listing and by `autogenerate`.

## The generated pages are committed, and they rot

They are checked in, so the mirror can disagree with `docs/` at any commit that
touched a source without re-running the sync. Before trusting a page here,
re-run the sync and check the diff is empty. After changing any source doc,
re-run the sync and commit the regenerated pages alongside it.

## House rules that apply here too

Prose follows the repository's conventions: sentence-case headings, no emoji, no
first person, and the vocabulary ban in `docs/design/SCOPE-BOUNDARY.md` §5.
`just lint-vocabulary` gates `internal/`, `web/src/` and `db/migrations/` only —
it does not read this directory, so nothing here is caught automatically.
