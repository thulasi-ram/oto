// Package api is the HTTP surface for delivery drills: start one against a
// source, poll it, and dispose of the synthetic rows it made.
//
// ⭐ THE POLL RETURNS A STAGED RESULT, NEVER A BOOLEAN. "It failed" is what the
// channel test already says and it is worth almost nothing; "the notification
// policy matched nothing" sends an operator straight to the screen that fixes it.
// That is the entire reason this surface exists rather than one more `ok: false`.
package api
