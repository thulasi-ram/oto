// Package rulematch recovers the originating Prometheus alerting rule for one alert, by decoding g0.expr out of generatorURL (primary, zero API calls) and by matching /api/v1/rules on alertname plus a non-templated label subset (fallback), reporting which strategy won and how confident it is.
package rulematch
