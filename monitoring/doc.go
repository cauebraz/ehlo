// Package monitoring builds ehlo's Grafana dashboards and Prometheus rules
// and renders them for a monitoring stack.
//
// Render(Default(), ...) writes the files committed under grafana/dashboards
// and prometheus/rules. A Config adapts them to another stack: a label
// selector for the ehlo targets, labels such as cluster kept by every
// aggregation, the Grafana datasource, uids, the alert state metric, and the
// Kubernetes objects that carry the rules and dashboards.
//
// The API is v0: it can change in any release until ehlo tags v1.
package monitoring
