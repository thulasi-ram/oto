// Package api is the Alertmanager webhook receiver: authenticate, bound, persist,
// enqueue, 202. No outbound call, nothing slow, and never a 4xx for anything
// transient.
package api
