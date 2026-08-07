// Package httpc is oto's shared outbound HTTP client: base-URL normalisation, bearer/basic/no auth, TLS options, per-attempt timeouts, bounded retry on 5xx and network errors only, response size caps and lenient JSON decoding, with every failure mapped onto a platform/errs Kind and a stable Code.
package httpc
