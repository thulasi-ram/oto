// Package netguard is oto's one Server-Side Request Forgery control.
//
// oto dials operator-supplied URLs — an AlertSource's Alertmanager and
// Prometheus, a webhook channel's receiver — from inside the operator's network.
// That is the textbook SSRF setup: a URL pointing at 169.254.169.254 turns a
// configuration field into cloud-credential exfiltration, and one pointing at
// 127.0.0.1 turns it into a way to reach oto's own admin surfaces from outside.
//
// ⭐ THE CHECK THAT MATTERS HAPPENS AT DIAL TIME, on the address the socket
// actually connected to. A pre-flight `LookupNetIP` followed by `client.Do` is
// TWO INDEPENDENT RESOLUTIONS, and an attacker-controlled name served with TTL 0
// that alternates public → 169.254.169.254 passes the first and is dialled at the
// second. `Guard.DialContext` closes that window because there is no second
// resolution to lose: the address it inspects is the address it hands to the
// kernel.
//
// CheckURL still exists, and is still worth calling at configuration time — but
// only for FAST FEEDBACK to a human who has just pasted a URL. It is not the
// control. Nothing in this package treats a passed CheckURL as permission to
// dial.
package netguard
