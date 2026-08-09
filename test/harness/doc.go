// Package harness is the ONE integration-test bootstrap for oto: a real
// Postgres via testcontainers, a FakeClock over platform/clock, fakes for the
// four true externals (Slack Web API, Alertmanager v2, Prometheus v1, outbound
// generic webhooks) and object builders for the fixtures every test needs.
//
// It exists because the same forty lines of testcontainers boilerplate were
// pasted into three unrelated files, and because ADR 0021 draws a hard line
// about what a test may fake:
//
//   - REAL, ALWAYS: Postgres, River, every service / repository / domain, the
//     chi router and the httpx stack. MOCKING A REPOSITORY IS NOT PERMITTED —
//     every invariant that matters here (dedup on the identity key, per-thread
//     sequence gating on advisory locks, the ON CONFLICT that IS the idempotency
//     mechanism) lives in the SQL, and a mocked DB agrees with whatever the code
//     does.
//   - FAKED, ONLY AT AN ADAPTER PORT THAT LEAVES THE PROCESS: the Slack Web API,
//     Alertmanager v2 and Prometheus v1 HTTP, and outbound webhook receivers.
//
// Every fake in this package is an httptest.Server speaking the upstream's real
// wire format, driven through oto's REAL client. Nothing here implements an oto
// port interface by hand: `alertmanager.Client`, `prometheus.Client` and the
// slack-go SDK are the things under test as much as the service above them, so
// the seam is the socket and not the Go interface.
//
// # Usage
//
//	func TestMain(m *testing.M) { harness.Main(m) }
//
//	func TestSomething(t *testing.T) {
//	    h := harness.New(t)
//	    org := h.Org()
//	    ...
//	}
package harness
