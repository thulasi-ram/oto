// Package ratelimit is oto's token bucket and the middleware that fronts the
// login endpoint with it.
//
// ⭐ RATE LIMITING THE LOGIN PATH IS A DENIAL-OF-SERVICE CONTROL AS MUCH AS AN
// AUTHENTICATION ONE. `identity/service.Login` runs argon2id at 19 MiB of memory
// per evaluation, and it runs it on EVERY path — including `DummyVerify` for an
// address that does not exist, deliberately, so that a stopwatch cannot tell a
// real account from an imaginary one. That uniform cost is the right call for
// enumeration and the wrong shape for load: unauthenticated traffic sent to
// `POST /api/v1/auth/login` allocates 19 MiB per in-flight request with no
// credential of any kind, so a few hundred concurrent requests are gigabytes of
// resident memory and every core busy. The limiter is what stops that, and the
// concurrency gate below is what bounds the worst case even under the limit.
//
// ⛔ IT IS NOT FOR `/ingest`. A 429 makes Alertmanager DELETE the notification
// permanently (SPEC §G.2, C4); that path sheds with 503 + Retry-After and has its
// own Shedder. Nothing in this package may be mounted in front of it.
package ratelimit
