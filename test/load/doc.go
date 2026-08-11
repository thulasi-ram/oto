// Package load holds oto's storm load cases: the tests that push a real
// Alertmanager storm through the real router, the real Postgres, the real job
// queue and a fake-but-conforming Slack, and record what happened.
//
// ⭐ WHY THIS PACKAGE EXISTS. The whole reason to run oto instead of
// Alertmanager's own Slack receiver is that oto is supposed to behave well when
// five hundred alerts fire at once. Until these tests existed nothing in the
// repository had ever pushed a large batch through
// `POST /api/v1/ingest/alertmanager/{source_id}` — the shedder, B17 chunking, the
// storm latch on `channels.storm_notice_at` and the per-thread ordering gate
// under hundreds of queued deliveries were all unexercised at scale, and
// Alertmanager does not retry a 4xx, so anything that degraded degraded into
// permanent silence (ADR 0007).
//
// # Gating: a build tag, not just -short
//
// ⛔ THE TESTS ARE BEHIND `//go:build load` AND THEY MUST STAY THERE. `just test`
// runs `go test -race -count=1 ./...` with NO `-short`, so a `testing.Short()`
// guard alone would make every one of these minutes-long cases CI-BLOCKING —
// which is how an expensive test gets deleted rather than fixed. The tag means
// `go test ./...` does not even compile them in, while
//
//	go test -tags load ./test/load/ -run TestStorm -v
//
// runs them on demand. `harness.New` additionally skips under `-short`, so the
// belt and the braces are both fastened.
//
// ⚠️ A tagged file is invisible to `go vet ./...` and to golangci-lint's default
// run. Check them explicitly with `go vet -tags load ./test/load/`.
//
// # Running them
//
//	# colima
//	DOCKER_HOST="unix://$HOME/.colima/default/docker.sock" \
//	TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock \
//	OTO_LOAD_RESULTS=/tmp/oto-load.json \
//	  go test -tags load ./test/load/ -run TestStorm -v -timeout 45m
//
// `OTO_LOAD_RESULTS` is optional; without it the numbers are only logged. ⚠️ It is
// APPENDED to, not overwritten, so it wants a scratch path — the checked-in
// artefact is `test/load/RESULTS.md`, transcribed from a run.
// `OTO_LOAD_LOG=warn` turns the container's own logger back on, for debugging.
//
// # What is asserted, and what is merely recorded
//
// ⛔ TIMINGS ARE RECORDED, NEVER ASSERTED. A latency budget enforced on a
// laptop, a CI runner and a 2-CPU colima VM is a budget that fails for reasons
// that have nothing to do with the code. The hard assertions are the invariants
// in `assertStormInvariants`: nothing silently lost, exactly one storm notice per
// channel, no duplicated or lost Slack call, an ordering gate that made it to the
// end of every thread, and rollups that are O(groups) rather than O(alerts).
//
// The measured numbers land in `test/load/RESULTS.md`, with the machine they were
// taken on stated, so a regression shows up as a diff rather than as a feeling.
package load
