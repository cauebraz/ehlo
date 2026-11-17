package monitoring

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Config adapts the dashboards and rules to a monitoring stack. A zero
// field takes its default; Default returns every default filled in.
type Config struct {
	// Selector holds PromQL label matchers, such as job="mail-ehlo",
	// inserted into every selector of an ehlo_ series in the rules and the
	// dashboards. Use labels every series of the target carries. The labels
	// of its equality matchers are kept by every recording rule and alert,
	// and select the recorded series and alerts the rules and dashboards
	// read, so renders for two selectors can share one Prometheus.
	Selector string `yaml:"selector"`
	// GroupBy lists labels, such as cluster or namespace, that every
	// recording rule and alert keeps, and that every dashboard offers as a
	// multi-select filter.
	GroupBy []string `yaml:"group_by"`
	// Datasource is the uid the dashboards' datasource variable selects
	// first. Empty selects Grafana's default datasource.
	Datasource string `yaml:"datasource"`
	// UIDPrefix starts every dashboard uid and file name. Without its
	// trailing hyphens it also names the rule groups, the Mimir namespace
	// and the PrometheusRule, so two renders in one stack need two prefixes.
	UIDPrefix string `yaml:"uid_prefix"`
	// AlertsMetric is the alert state series the dashboards read: ALERTS
	// from Prometheus or Mimir, or GRAFANA_ALERTS from Grafana-managed
	// alerting with the Prometheus state history backend.
	AlertsMetric string `yaml:"alerts_metric"`
	// RuleInterval is the evaluation interval of both rule groups.
	RuleInterval string `yaml:"rule_interval"`
	// RuleLabels are extra labels on every alert. component: ehlo is always
	// set on every rule.
	RuleLabels map[string]string `yaml:"rule_labels"`
	// RunbookURL is the runbook_url annotation of each alert. {{alert}}
	// becomes the lowercased alert name.
	RunbookURL string `yaml:"runbook_url"`
	// DashboardURL is the Grafana base URL, such as
	// https://grafana.example.com. When set, each alert carries a
	// dashboard_url annotation to its dashboard.
	DashboardURL string `yaml:"dashboard_url"`
	// FolderUID is the Grafana folder of the GrafanaManifest objects.
	FolderUID string `yaml:"folder_uid"`
	// Kubernetes is the metadata of the PrometheusRule, ConfigMap and
	// GrafanaManifest objects.
	Kubernetes Kubernetes `yaml:"kubernetes"`
	// ConfigMapLabel is the label the Grafana dashboard sidecar watches.
	ConfigMapLabel map[string]string `yaml:"configmap_label"`
}

// Kubernetes is the metadata of the rendered Kubernetes objects.
type Kubernetes struct {
	Namespace string `yaml:"namespace"`
	// Labels go on every object, for example release:
	// kube-prometheus-stack so the operator selects the PrometheusRule.
	Labels map[string]string `yaml:"labels"`
	// Annotations go on every object, for example grafana_folder for the
	// dashboard sidecar.
	Annotations map[string]string `yaml:"annotations"`
	// InstanceSelector selects the Grafana instances of the
	// GrafanaManifest objects.
	InstanceSelector map[string]string `yaml:"instance_selector"`
}

// Default values.
const (
	DefaultUIDPrefix    = "ehlo-"
	DefaultAlertsMetric = "ALERTS"
	DefaultRuleInterval = "15s"
	DefaultRunbookURL   = "https://github.com/cauebraz/ehlo/blob/main/docs/alerts.md#{{alert}}"
)

// Default returns the configuration of the committed files.
func Default() Config { return Config{}.withDefaults() }

// withDefaults fills every zero field with its default. A nil map takes the
// default; an empty one stays empty.
func (c Config) withDefaults() Config {
	c.UIDPrefix = cmp.Or(c.UIDPrefix, DefaultUIDPrefix)
	c.AlertsMetric = cmp.Or(c.AlertsMetric, DefaultAlertsMetric)
	c.RuleInterval = cmp.Or(c.RuleInterval, DefaultRuleInterval)
	c.RunbookURL = cmp.Or(c.RunbookURL, DefaultRunbookURL)
	if c.ConfigMapLabel == nil {
		c.ConfigMapLabel = map[string]string{"grafana_dashboard": "1"}
	}
	if c.Kubernetes.InstanceSelector == nil {
		c.Kubernetes.InstanceSelector = map[string]string{"dashboards": "grafana"}
	}
	return c
}

// LoadConfig reads a YAML file and fills the keys it omits with their
// defaults. An unknown key is an error.
func LoadConfig(path string) (Config, error) {
	var cfg Config
	b, err := os.ReadFile(path) // #nosec G304 -- the path is the operator's own config file
	if err != nil {
		return cfg, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	cfg = cfg.withDefaults()
	return cfg, cfg.Validate()
}

var (
	labelNameRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	matcherList = regexp.MustCompile(`^\s*[a-zA-Z_][a-zA-Z0-9_]*\s*(=~|!~|!=|=)\s*"(?:[^"\\]|\\.)*"\s*(,\s*[a-zA-Z_][a-zA-Z0-9_]*\s*(=~|!~|!=|=)\s*"(?:[^"\\]|\\.)*"\s*)*,?\s*$`)
	durationRe  = regexp.MustCompile(`^([0-9]+(ms|s|m|h|d|w|y))+$`)
	uidPrefixRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
)

// Validate reports the first invalid field.
func (c Config) Validate() error {
	if c.Selector != "" && !matcherList.MatchString(c.Selector) {
		return fmt.Errorf("selector %q is not a list of label matchers such as job=\"ehlo\"", c.Selector)
	}
	seen := map[string]bool{}
	for _, l := range c.GroupBy {
		if !labelNameRe.MatchString(l) || strings.HasPrefix(l, "__") {
			return fmt.Errorf("group_by: %q is not a label name", l)
		}
		if slices.Contains(reservedLabels, l) {
			return fmt.Errorf("group_by: %q is a label of ehlo's series or alerts", l)
		}
		if seen[l] {
			return fmt.Errorf("group_by: %q repeats", l)
		}
		seen[l] = true
	}
	if !uidPrefixRe.MatchString(c.UIDPrefix) {
		return fmt.Errorf("uid_prefix %q must start with a lowercase letter or digit and hold only lowercase letters, digits and -, as it names Kubernetes objects", c.UIDPrefix)
	}
	if n := len(c.UIDPrefix) + len("reputation"); n > 40 {
		return fmt.Errorf("uid_prefix %q makes uids of %d characters, Grafana allows 40", c.UIDPrefix, n)
	}
	if c.AlertsMetric != "ALERTS" && c.AlertsMetric != "GRAFANA_ALERTS" {
		return fmt.Errorf("alerts_metric %q: want ALERTS or GRAFANA_ALERTS", c.AlertsMetric)
	}
	if !durationRe.MatchString(c.RuleInterval) {
		return fmt.Errorf("rule_interval %q is not a Prometheus duration such as 15s", c.RuleInterval)
	}
	kept := c.keptLabels()
	for k := range c.RuleLabels {
		if !labelNameRe.MatchString(k) || strings.HasPrefix(k, "__") {
			return fmt.Errorf("rule_labels: %q is not a label name", k)
		}
		if slices.Contains(reservedLabels, k) {
			return fmt.Errorf("rule_labels: %q is set by ehlo or Prometheus", k)
		}
		if slices.Contains(kept, k) {
			return fmt.Errorf("rule_labels: %q would overwrite a label kept from group_by or the selector", k)
		}
	}
	if c.RunbookURL == "" {
		return errors.New("runbook_url is empty")
	}
	if c.DashboardURL != "" {
		u, err := url.Parse(c.DashboardURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Fragment != "" {
			return fmt.Errorf("dashboard_url %q is not an http or https URL with a host and no fragment", c.DashboardURL)
		}
	}
	if len(c.ConfigMapLabel) == 0 {
		return errors.New("configmap_label is empty")
	}
	return c.validateKubernetes()
}

// validateKubernetes checks the Kubernetes metadata against the API server's
// rules for namespaces, label keys and values, and annotation keys.
func (c Config) validateKubernetes() error {
	if ns := c.Kubernetes.Namespace; ns != "" && !isDNSLabel(ns) {
		return fmt.Errorf("kubernetes.namespace %q is not a DNS-1123 label", ns)
	}
	for field, labels := range map[string]map[string]string{
		"kubernetes.labels":            c.Kubernetes.Labels,
		"kubernetes.instance_selector": c.Kubernetes.InstanceSelector,
		"configmap_label":              c.ConfigMapLabel,
	} {
		for k, v := range labels {
			if !isQualifiedName(k) {
				return fmt.Errorf("%s: key %q is not a Kubernetes qualified name", field, k)
			}
			if !isLabelValue(v) {
				return fmt.Errorf("%s: value %q of %s is not a Kubernetes label value", field, v, k)
			}
		}
	}
	for k := range c.Kubernetes.Annotations {
		if !isQualifiedName(k) {
			return fmt.Errorf("kubernetes.annotations: key %q is not a Kubernetes qualified name", k)
		}
	}
	return nil
}

var (
	dnsLabelRe      = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	dnsSubdomainRe  = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
	qualifiedNameRe = regexp.MustCompile(`^([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]$`)
)

func isDNSLabel(s string) bool { return len(s) <= 63 && dnsLabelRe.MatchString(s) }

// isQualifiedName reports whether s is a label or annotation key: a name
// with an optional DNS subdomain prefix and a slash.
func isQualifiedName(s string) bool {
	prefix, name, ok := strings.Cut(s, "/")
	if !ok {
		name = prefix
	} else if prefix == "" || len(prefix) > 253 || !dnsSubdomainRe.MatchString(prefix) {
		return false
	}
	return len(name) <= 63 && qualifiedNameRe.MatchString(name)
}

func isLabelValue(s string) bool { return s == "" || (len(s) <= 63 && qualifiedNameRe.MatchString(s)) }

// reservedLabels are the labels of ehlo's series, of the alert state series
// of Prometheus and Grafana, and of ehlo's alerts. group_by and rule_labels
// cannot use them.
var reservedLabels = []string{
	"route", "source", "provider", "type", "result", "attempt", "class", "code", "category", "kind",
	"audience", "version", "trust", "spf", "dkim", "state", "le",
	"alertname", "alertstate", "severity", "component", "grafana_alertstate", "grafana_rule_uid",
}

// scopeLabels are the labels of the selector's equality matchers. Every
// recorded series and alert keeps them, so renders for two selectors do not
// collide in one Prometheus.
func scopeLabels(sel string) []string {
	var out []string
	for _, m := range scopeMatchers(sel) {
		name, _, _ := strings.Cut(m, "=")
		if name = strings.TrimSpace(name); !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

// scopeMatchers are the selector's equality matchers.
func scopeMatchers(sel string) []string {
	var out []string
	for _, m := range selectorMatchers(sel) {
		name, rest, _ := strings.Cut(m, "=")
		if !strings.HasSuffix(name, "!") && !strings.HasPrefix(rest, "~") {
			out = append(out, m)
		}
	}
	return out
}

// keptLabels are the labels every recording rule and alert keeps: group_by
// first, then the scope labels group_by lacks.
func (c Config) keptLabels() []string {
	out := slices.Clone(c.GroupBy)
	for _, l := range scopeLabels(c.Selector) {
		if !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	return out
}
