// Package design holds the gates SPEC §M.7 declares over the web tree: that
// saturated Tier-B colour is spent on alert state and nowhere else, and that the
// Slack palette (§H.2) and the oto UI tokens (§M.4/§M.5) stay two separate systems.
//
// They live in Go, run by `go test ./...`, because they are checks ON the front
// end rather than checks the front end runs: no bundler, no jsdom, no browser,
// and nothing to install. The vitest suite owns everything that needs the
// stylesheet compiled — `web/src/index.css.test.ts`, `web/src/design/*.test.ts`.
package design
