package monitoring

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"go.yaml.in/yaml/v3"

	"github.com/cauebraz/ehlo/internal/metrics"
	"github.com/cauebraz/ehlo/internal/warmup"
)

// renderedDoc decodes one dashboard rendered for cfg.
func renderedDoc(t *testing.T, cfg Config, d Dashboard) obj {
	t.Helper()
	b, err := DashboardJSON(cfg, d)
	if err != nil {
		t.Fatalf("%s: %v", d.Name, err)
	}
	var out obj
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("%s: %v", d.Name, err)
	}
	return out
}

// renderedExprs returns every PromQL query of d rendered for cfg: panels,
// variables and annotations.
func renderedExprs(t *testing.T, cfg Config, d Dashboard) map[string]string {
	t.Helper()
	out := map[string]string{}
	spec := field[obj](t, renderedDoc(t, cfg, d), "spec", d.Name)
	for name, e := range field[obj](t, spec, "elements", d.Name) {
		p := field[obj](t, e.(obj), "spec", name)
		queries := field[[]any](t, field[obj](t, field[obj](t, p, "data", name), "spec", name), "queries", name)
		for i, q := range queries {
			qs := field[obj](t, field[obj](t, field[obj](t, q.(obj), "spec", name), "query", name), "spec", name)
			out[fmt.Sprintf("%s: %v query %d", d.Name, p["title"], i)] = field[string](t, qs, "expr", name)
		}
	}
	for _, v := range field[[]any](t, spec, "variables", d.Name) {
		vs := field[obj](t, v.(obj), "spec", d.Name)
		if q, ok := vs["query"].(obj); ok {
			out[fmt.Sprintf("%s: variable %v", d.Name, vs["name"])] = field[string](t, field[obj](t, q, "spec", d.Name), "query", d.Name)
		}
	}
	for _, a := range field[[]any](t, spec, "annotations", d.Name) {
		as := field[obj](t, a.(obj), "spec", d.Name)
		out[fmt.Sprintf("%s: annotation %v", d.Name, as["name"])] = field[string](t, field[obj](t, as, "legacyOptions", d.Name), "expr", d.Name)
	}
	return out
}

// renderedLegends returns every legend of d rendered for cfg.
func renderedLegends(t *testing.T, cfg Config, d Dashboard) []string {
	t.Helper()
	var out []string
	spec := field[obj](t, renderedDoc(t, cfg, d), "spec", d.Name)
	for name, e := range field[obj](t, spec, "elements", d.Name) {
		p := field[obj](t, e.(obj), "spec", name)
		for _, q := range field[[]any](t, field[obj](t, field[obj](t, p, "data", name), "spec", name), "queries", name) {
			qs := field[obj](t, field[obj](t, field[obj](t, q.(obj), "spec", name), "query", name), "spec", name)
			out = append(out, field[string](t, qs, "legendFormat", name))
		}
	}
	return out
}

var (
	seriesSelectorRe  = regexp.MustCompile(`\b(ehlo_[a-z0-9_]+|ehlo:[a-z0-9_:]+|ALERTS)\b(\{[^}]*\})?`)
	groupingClauseRe  = regexp.MustCompile(`\b(by|on) \(([^)]*)\)`)
	bareAggregationRe = regexp.MustCompile(`\b(sum|max|min|avg|count|group|topk)\(`)
)

func adopterConfig() Config {
	cfg := Default()
	cfg.Selector = `job="mail-ehlo", namespace="mail"`
	cfg.GroupBy = []string{"cluster"}
	return cfg
}

func TestSelectorAndGroupByReachEveryDashboardQuery(t *testing.T) {
	cfg := adopterConfig()
	for _, d := range Dashboards() {
		for where, expr := range renderedExprs(t, cfg, d) {
			for _, m := range seriesSelectorRe.FindAllStringSubmatch(expr, -1) {
				name, braces := m[1], m[2]
				if strings.HasPrefix(name, "ehlo_") {
					for _, want := range []string{`job="mail-ehlo"`, `namespace="mail"`} {
						if !strings.Contains(braces, want) {
							t.Errorf("%s: %s lacks %s", where, m[0], want)
						}
					}
				}
				isClusterVar := strings.HasSuffix(where, "variable cluster")
				if !isClusterVar && !strings.Contains(braces, `cluster=~"$cluster"`) {
					t.Errorf("%s: %s lacks the cluster filter", where, m[0])
				}
			}
			for _, m := range groupingClauseRe.FindAllStringSubmatch(expr, -1) {
				if !strings.HasPrefix(m[2], "cluster") {
					t.Errorf("%s: %s does not keep cluster", where, m[0])
				}
			}
		}
		for _, l := range renderedLegends(t, cfg, d) {
			if l != "" && !strings.HasPrefix(l, "{{cluster}} ") {
				t.Errorf("%s: legend %q does not name the cluster", d.Name, l)
			}
		}
		var names []string
		spec := field[obj](t, renderedDoc(t, cfg, d), "spec", d.Name)
		for _, v := range field[[]any](t, spec, "variables", d.Name) {
			names = append(names, fmt.Sprint(field[obj](t, v.(obj), "spec", d.Name)["name"]))
		}
		if len(names) < 3 || names[0] != "datasource" || names[1] != "cluster" || names[2] != "source" {
			t.Errorf("%s: variables %v, want datasource, cluster, source first", d.Name, names)
		}
	}
}

func TestSelectorAndGroupByReachEveryRule(t *testing.T) {
	cfg := adopterConfig()
	for _, r := range renderedRules(t, cfg) {
		where := r.Record + r.Alert
		for _, m := range seriesSelectorRe.FindAllStringSubmatch(r.Expr, -1) {
			if strings.HasPrefix(m[1], "ehlo_") && !strings.Contains(m[2], `job="mail-ehlo", namespace="mail"`) {
				t.Errorf("%s: %s lacks the selector", where, m[0])
			}
			if strings.HasPrefix(m[1], "ehlo:") && m[2] != "" && strings.Contains(m[2], "$") {
				t.Errorf("%s: %s holds a dashboard variable", where, m[0])
			}
		}
		clauses := groupingClauseRe.FindAllStringSubmatch(r.Expr, -1)
		if len(clauses) == 0 && strings.Contains(r.Expr, "ehlo_") {
			t.Errorf("%s: reads raw series without a by or on clause, so it cannot keep cluster: %s", where, r.Expr)
		}
		if m := bareAggregationRe.FindString(r.Expr); m != "" {
			t.Errorf("%s: %s aggregates away cluster", where, m)
		}
		for _, m := range clauses {
			if !strings.HasPrefix(m[2], "cluster") {
				t.Errorf("%s: %s does not keep cluster", where, m[0])
			}
		}
		if strings.Contains(r.Expr, `job="ehlo"`) {
			t.Errorf("%s names the demo job", where)
		}
	}
}

func TestDefaultRenderAddsNoMatcher(t *testing.T) {
	for _, d := range Dashboards() {
		for where, expr := range renderedExprs(t, Default(), d) {
			if strings.Contains(expr, "cluster") || strings.Contains(expr, "job=") {
				t.Errorf("%s: %s", where, expr)
			}
		}
		for _, l := range renderedLegends(t, Default(), d) {
			if strings.Contains(l, "cluster") {
				t.Errorf("%s: legend %q", d.Name, l)
			}
		}
	}
}

func TestDatasource(t *testing.T) {
	current := func(cfg Config) obj {
		spec := field[obj](t, renderedDoc(t, cfg, Dashboards()[0]), "spec", "overview")
		v := field[obj](t, field[[]any](t, spec, "variables", "overview")[0].(obj), "spec", "datasource")
		return field[obj](t, v, "current", "datasource")
	}
	if c := current(Default()); c["value"] != "default" || c["text"] != "default" {
		t.Errorf("default datasource current = %v, want the default option so Grafana selects its default datasource", c)
	}
	cfg := Default()
	cfg.Datasource = "mimir-prod"
	if c := current(cfg); c["value"] != "mimir-prod" {
		t.Errorf("datasource current = %v, want mimir-prod", c)
	}
}

func TestStatsDoNotClaimACauseForNoData(t *testing.T) {
	for _, d := range Dashboards() {
		b, err := DashboardJSON(Default(), d)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte("No data yet")) {
			t.Errorf("%s: a stat says No data yet, which hides query errors", d.Name)
		}
	}
}

func TestUIDPrefix(t *testing.T) {
	cfg := Default()
	cfg.UIDPrefix = "mail-"
	files, err := Files(cfg, FormatAll)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range Dashboards() {
		b := files[DashboardsDir+"/mail-"+d.Name+".json"]
		if b == nil {
			t.Errorf("no file for %s", d.Name)
			continue
		}
		if bytes.Contains(b, []byte("/d/ehlo-")) || bytes.Contains(b, []byte(`"ehlo-`)) {
			t.Errorf("%s still links to an ehlo- uid", d.Name)
		}
	}
	for _, r := range renderedRules(t, cfg) {
		if r.Alert != "" && !strings.HasPrefix(r.Annotations["dashboard_uid"], "mail-") {
			t.Errorf("%s dashboard_uid = %q", r.Alert, r.Annotations["dashboard_uid"])
		}
	}
}

func TestRuleKnobs(t *testing.T) {
	cfg := Default()
	cfg.RuleInterval = "1m"
	cfg.RuleLabels = map[string]string{"team": "mail"}
	cfg.RunbookURL = "https://wiki.example.com/ehlo/{{alert}}"
	cfg.DashboardURL = "https://grafana.example.com/"
	for _, g := range renderedGroups(t, cfg) {
		if g.Interval != "1m" {
			t.Errorf("%s interval = %q", g.Name, g.Interval)
		}
		for _, r := range g.Rules {
			if r.Alert == "" {
				if len(r.Labels) > 0 || len(r.Annotations) > 0 {
					t.Errorf("%s: a recording rule carries labels or annotations", r.Record)
				}
				continue
			}
			if r.Labels["team"] != "mail" || r.Labels["component"] != "ehlo" || r.Labels["severity"] == "" {
				t.Errorf("%s labels = %v", r.Alert, r.Labels)
			}
			if want := "https://wiki.example.com/ehlo/" + strings.ToLower(r.Alert); r.Annotations["runbook_url"] != want {
				t.Errorf("%s runbook_url = %q, want %q", r.Alert, r.Annotations["runbook_url"], want)
			}
			if want := "https://grafana.example.com/d/" + r.Annotations["dashboard_uid"]; r.Annotations["dashboard_url"] != want {
				t.Errorf("%s dashboard_url = %q, want %q", r.Alert, r.Annotations["dashboard_url"], want)
			}
		}
	}
	for _, r := range renderedRules(t, Default()) {
		if _, ok := r.Annotations["dashboard_url"]; ok {
			t.Errorf("%s has a dashboard_url without a Grafana URL", r.Alert)
		}
	}
}

func TestTelemetryAbsentIsJobAgnostic(t *testing.T) {
	for _, cfg := range []Config{Default(), adopterConfig()} {
		for _, r := range renderedRules(t, cfg) {
			if r.Alert != "EhloTelemetryAbsent" {
				continue
			}
			if strings.Contains(r.Expr, "job=") && !strings.Contains(r.Expr, `job="mail-ehlo"`) {
				t.Errorf("expr names a job: %s", r.Expr)
			}
			if !strings.Contains(r.Expr, "ehlo_build_info") {
				t.Errorf("expr does not read ehlo_build_info: %s", r.Expr)
			}
		}
	}
}

// yamlDocs decodes every document of a YAML stream.
func yamlDocs(t *testing.T, b []byte) []map[string]any {
	t.Helper()
	dec := yaml.NewDecoder(bytes.NewReader(b))
	var out []map[string]any
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, doc)
	}
}

func dig(t *testing.T, v any, path ...string) any {
	t.Helper()
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("%v: %T is not a mapping", path, v)
		}
		v = m[p]
	}
	return v
}

func TestKubernetesFormats(t *testing.T) {
	cfg := Default()
	cfg.Kubernetes = Kubernetes{
		Namespace:        "monitoring",
		Labels:           map[string]string{"release": "kube-prometheus-stack"},
		Annotations:      map[string]string{"grafana_folder": "Email"},
		InstanceSelector: map[string]string{"dashboards": "shared"},
	}
	cfg.FolderUID = "email"
	files, err := Files(cfg, FormatAll)
	if err != nil {
		t.Fatal(err)
	}

	rule := yamlDocs(t, files[PrometheusRuleFile])
	if len(rule) != 1 || rule[0]["apiVersion"] != "monitoring.coreos.com/v1" || rule[0]["kind"] != "PrometheusRule" {
		t.Fatalf("PrometheusRule = %v", rule)
	}
	if dig(t, rule[0], "metadata", "namespace") != "monitoring" || dig(t, rule[0], "metadata", "labels", "release") != "kube-prometheus-stack" {
		t.Errorf("PrometheusRule metadata = %v", rule[0]["metadata"])
	}
	if groups, _ := dig(t, rule[0], "spec", "groups").([]any); len(groups) != 2 {
		t.Errorf("PrometheusRule has %d groups, want 2", len(groups))
	}

	cms := yamlDocs(t, files[ConfigMapsFile])
	if len(cms) != len(Dashboards()) {
		t.Fatalf("%d ConfigMaps for %d dashboards", len(cms), len(Dashboards()))
	}
	for i, d := range Dashboards() {
		cm := cms[i]
		if cm["kind"] != "ConfigMap" || dig(t, cm, "metadata", "labels", "grafana_dashboard") != "1" || dig(t, cm, "metadata", "labels", "release") != "kube-prometheus-stack" {
			t.Errorf("%s ConfigMap metadata = %v", d.Name, cm["metadata"])
		}
		if dig(t, cm, "metadata", "annotations", "grafana_folder") != "Email" {
			t.Errorf("%s ConfigMap annotations = %v", d.Name, dig(t, cm, "metadata", "annotations"))
		}
		data, _ := dig(t, cm, "data", uid(d)+".json").(string)
		if data != string(files[DashboardsDir+"/"+uid(d)+".json"]) {
			t.Errorf("%s ConfigMap key %s.json differs from the dashboard file", d.Name, uid(d))
		}
		if n := len(data); n > 256*1024 {
			t.Errorf("%s ConfigMap holds %d bytes, over what kubectl apply stores", d.Name, n)
		}
	}

	ms := yamlDocs(t, files[GrafanaManifests])
	if len(ms) != len(Dashboards()) {
		t.Fatalf("%d GrafanaManifests for %d dashboards", len(ms), len(Dashboards()))
	}
	for i, d := range Dashboards() {
		m := ms[i]
		if m["apiVersion"] != "grafana.integreatly.org/v1beta1" || m["kind"] != "GrafanaManifest" {
			t.Errorf("%s: %v %v", d.Name, m["apiVersion"], m["kind"])
		}
		if dig(t, m, "spec", "instanceSelector", "matchLabels", "dashboards") != "shared" {
			t.Errorf("%s instanceSelector = %v", d.Name, dig(t, m, "spec", "instanceSelector"))
		}
		tmpl := dig(t, m, "spec", "template")
		if dig(t, tmpl, "apiVersion") != "dashboard.grafana.app/v2" || dig(t, tmpl, "kind") != "Dashboard" || dig(t, tmpl, "metadata", "name") != uid(d) {
			t.Errorf("%s template header = %v %v %v", d.Name, dig(t, tmpl, "apiVersion"), dig(t, tmpl, "kind"), dig(t, tmpl, "metadata"))
		}
		if dig(t, tmpl, "metadata", "annotations", "grafana.app/folder") != "email" {
			t.Errorf("%s template folder = %v", d.Name, dig(t, tmpl, "metadata"))
		}
		var want map[string]any
		if err := json.Unmarshal(files[DashboardsDir+"/"+uid(d)+".json"], &want); err != nil {
			t.Fatal(err)
		}
		gotJSON, err := json.Marshal(dig(t, tmpl, "spec"))
		if err != nil {
			t.Fatal(err)
		}
		wantJSON, err := json.Marshal(want["spec"])
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(gotJSON, wantJSON) {
			t.Errorf("%s template spec differs from the dashboard file", d.Name)
		}
	}

	mimir := yamlDocs(t, files[MimirFile])
	if len(mimir) != 1 || mimir[0]["namespace"] != "ehlo" {
		t.Fatalf("mimir file = %v", mimir)
	}
	if groups, _ := mimir[0]["groups"].([]any); len(groups) != 2 {
		t.Errorf("mimir file has %d groups, want 2", len(groups))
	}
}

func TestRenderWritesTheLayout(t *testing.T) {
	dir := t.TempDir()
	if err := Render(Default(), FormatAll, dir); err != nil {
		t.Fatal(err)
	}
	want := []string{"kubernetes/dashboards-configmaps.yaml", "kubernetes/grafana-manifests.yaml", "kubernetes/prometheusrule.yaml", "mimir/ehlo.yaml", "rules/ehlo-alerts.yml", "rules/ehlo-recording.yml"}
	for _, d := range Dashboards() {
		want = append(want, "dashboards/"+uid(d)+".json")
	}
	var got []string
	err := filepath.WalkDir(dir, func(path string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		got = append(got, filepath.ToSlash(rel))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("files = %v, want %v", got, want)
	}
	if err := Render(Default(), "classic", dir); err == nil {
		t.Error("an unknown format renders")
	}
}

func TestValidate(t *testing.T) {
	for name, edit := range map[string]func(*Config){
		"selector without quotes":  func(c *Config) { c.Selector = "job=ehlo" },
		"selector with braces":     func(c *Config) { c.Selector = `{job="ehlo"}` },
		"group_by not a label":     func(c *Config) { c.GroupBy = []string{"k8s-cluster"} },
		"group_by an ehlo label":   func(c *Config) { c.GroupBy = []string{"source"} },
		"group_by repeated":        func(c *Config) { c.GroupBy = []string{"cluster", "cluster"} },
		"uid prefix with slash":    func(c *Config) { c.UIDPrefix = "a/b" },
		"uid prefix too long":      func(c *Config) { c.UIDPrefix = strings.Repeat("x", 31) },
		"alerts metric":            func(c *Config) { c.AlertsMetric = "alerts" },
		"rule interval":            func(c *Config) { c.RuleInterval = "15" },
		"rule label component":     func(c *Config) { c.RuleLabels = map[string]string{"component": "x"} },
		"dashboard url not a URL":  func(c *Config) { c.DashboardURL = "grafana.example.com" },
		"configmap label emptied":  func(c *Config) { c.ConfigMapLabel = map[string]string{} },
		"rule label not a label":   func(c *Config) { c.RuleLabels = map[string]string{"a-b": "x"} },
		"selector with a trailing": func(c *Config) { c.Selector = `job="ehlo" or` },
		"rule label source":        func(c *Config) { c.RuleLabels = map[string]string{"source": "x"} },
		"rule label alertname":     func(c *Config) { c.RuleLabels = map[string]string{"alertname": "x"} },
		"rule label is group_by":   func(c *Config) { c.GroupBy = []string{"cluster"}; c.RuleLabels = map[string]string{"cluster": "x"} },
		"rule label is scoped":     func(c *Config) { c.Selector = `job="x"`; c.RuleLabels = map[string]string{"job": "y"} },
		"group_by reserved":        func(c *Config) { c.GroupBy = []string{"__name__"} },
		"uid prefix uppercase":     func(c *Config) { c.UIDPrefix = "Mail-" },
		"uid prefix underscore":    func(c *Config) { c.UIDPrefix = "mail_" },
		"uid prefix leading dash":  func(c *Config) { c.UIDPrefix = "-mail" },
		"dashboard url no host":    func(c *Config) { c.DashboardURL = "https://" },
		"dashboard url fragment":   func(c *Config) { c.DashboardURL = "https://g.example.com/#x" },
		"namespace uppercase":      func(c *Config) { c.Kubernetes.Namespace = "Bad_NS" },
		"namespace too long":       func(c *Config) { c.Kubernetes.Namespace = strings.Repeat("a", 64) },
		"label key with space":     func(c *Config) { c.Kubernetes.Labels = map[string]string{"a b": "x"} },
		"label key two slashes":    func(c *Config) { c.Kubernetes.Labels = map[string]string{"x/y/z": "x"} },
		"label key bad prefix":     func(c *Config) { c.Kubernetes.Labels = map[string]string{"Example.com/a": "x"} },
		"label value with space":   func(c *Config) { c.Kubernetes.Labels = map[string]string{"a": "x y"} },
		"label value too long":     func(c *Config) { c.Kubernetes.Labels = map[string]string{"a": strings.Repeat("x", 64)} },
		"annotation key":           func(c *Config) { c.Kubernetes.Annotations = map[string]string{"a b": "x"} },
		"configmap label key":      func(c *Config) { c.ConfigMapLabel = map[string]string{"x/y/z": "1"} },
		"instance selector value":  func(c *Config) { c.Kubernetes.InstanceSelector = map[string]string{"a": "-x"} },
	} {
		cfg := Default()
		edit(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: Validate passed", name)
		}
	}
	for _, l := range []string{"route", "source", "provider", "type", "result", "attempt", "class", "code", "category", "kind",
		"audience", "version", "trust", "spf", "dkim", "state", "le", "severity", "component", "alertname", "alertstate",
		"grafana_alertstate", "grafana_rule_uid"} {
		cfg := Default()
		cfg.GroupBy = []string{l}
		if err := cfg.Validate(); err == nil {
			t.Errorf("group_by %s: Validate passed", l)
		}
	}
	cfg := Default()
	cfg.Selector = `job=~"mail-ehlo|ehlo", namespace!="test",`
	cfg.GroupBy = []string{"cluster", "namespace"}
	cfg.RuleLabels = map[string]string{"team": "mail"}
	cfg.DashboardURL = "https://grafana.example.com/grafana?orgId=2"
	cfg.UIDPrefix = "mail-1-"
	cfg.Kubernetes = Kubernetes{
		Namespace:        "monitoring",
		Labels:           map[string]string{"app.kubernetes.io/part-of": "ehlo", "release": "", "a": "x_y.z"},
		Annotations:      map[string]string{"grafana_folder": "Email / SMTP"},
		InstanceSelector: map[string]string{"dashboards": "grafana"},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("valid config: %v", err)
	}
}

// collectors is a Registerer that keeps what ehlo registers.
type collectors []prometheus.Collector

func (c *collectors) Register(col prometheus.Collector) error   { *c = append(*c, col); return nil }
func (c *collectors) MustRegister(cols ...prometheus.Collector) { *c = append(*c, cols...) }
func (c *collectors) Unregister(prometheus.Collector) bool      { return false }

var variableLabelsRe = regexp.MustCompile(`variableLabels: \{([^}]*)\}`)

func TestMetricLabelsAreReserved(t *testing.T) {
	var cols collectors
	metrics.New(&cols, "test")
	eng, err := warmup.New(warmup.DefaultPolicy(), warmup.Targets{Default: 1000}, []string{"gmail"})
	if err != nil {
		t.Fatal(err)
	}
	cols.MustRegister(metrics.NewWarmup(eng.Lanes))
	ch := make(chan *prometheus.Desc, 256)
	go func() {
		for _, c := range cols {
			c.Describe(ch)
		}
		close(ch)
	}()
	seen := 0
	for d := range ch {
		m := variableLabelsRe.FindStringSubmatch(d.String())
		if m == nil {
			t.Fatalf("cannot read the labels of %s", d)
		}
		for _, l := range strings.Split(m[1], ",") {
			if l = strings.TrimSpace(l); l == "" {
				continue
			}
			seen++
			if !slices.Contains(reservedLabels, l) {
				t.Errorf("%s label %s is not in reservedLabels", d, l)
			}
		}
	}
	if seen == 0 {
		t.Fatal("no label found")
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	cfg, err := LoadConfig(write("full.yaml", `selector: job="mail-ehlo"
group_by: [cluster]
datasource: mimir-prod
alerts_metric: GRAFANA_ALERTS
rule_labels: {team: mail}
kubernetes:
  namespace: monitoring
  labels: {release: kube-prometheus-stack}
configmap_label: {grafana_dashboard: "true"}
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Selector != `job="mail-ehlo"` || !slices.Equal(cfg.GroupBy, []string{"cluster"}) || cfg.Datasource != "mimir-prod" ||
		cfg.AlertsMetric != "GRAFANA_ALERTS" || cfg.RuleLabels["team"] != "mail" || cfg.Kubernetes.Namespace != "monitoring" ||
		cfg.ConfigMapLabel["grafana_dashboard"] != "true" || len(cfg.ConfigMapLabel) != 1 {
		t.Errorf("loaded %+v", cfg)
	}
	if cfg.UIDPrefix != DefaultUIDPrefix || cfg.RuleInterval != DefaultRuleInterval || cfg.Kubernetes.InstanceSelector["dashboards"] != "grafana" {
		t.Errorf("omitted keys lost their defaults: %+v", cfg)
	}
	if _, err := LoadConfig(write("unknown.yaml", "selectr: x\n")); err == nil {
		t.Error("an unknown key loads")
	}
	if _, err := LoadConfig(write("bad.yaml", "alerts_metric: alerts\n")); err == nil {
		t.Error("an invalid value loads")
	}
	empty, err := LoadConfig(write("empty.yaml", ""))
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(empty) != fmt.Sprint(Default()) {
		t.Errorf("empty file = %+v, want the defaults", empty)
	}
}

func TestInject(t *testing.T) {
	m := matchers{raw: []string{`job="x"`}, recorded: []string{`cluster=~"$cluster"`}, alerts: []string{`cluster=~"$cluster"`}, alertsMetric: "GRAFANA_ALERTS"}
	for in, want := range map[string]string{
		`sum(rate(ehlo_a_total[5m]))`:                    `sum(rate(ehlo_a_total{job="x"}[5m]))`,
		`ehlo_a{b="c"} / ehlo_b{}`:                       `ehlo_a{b="c", job="x"} / ehlo_b{job="x"}`,
		`ehlo:r:ratio{s=~"$s"} > 0`:                      `ehlo:r:ratio{s=~"$s", cluster=~"$cluster"} > 0`,
		`max(ALERTS{component="ehlo"} or vector(0))`:     `max(GRAFANA_ALERTS{component="ehlo", cluster=~"$cluster"} or vector(0))`,
		`label_values(ehlo_smtp_attempts_total, source)`: `label_values(ehlo_smtp_attempts_total{job="x"}, source)`,
		`up == 0`: `up == 0`,
	} {
		if got := m.inject(in); got != want {
			t.Errorf("inject(%s) = %s, want %s", in, got, want)
		}
	}
	for in, want := range map[string]string{
		`sum by (source) (x) / on (source, provider) y`: `sum by (cluster, ns, source) (x) / on (cluster, ns, source, provider) y`,
		`a unless on () b`:                     `a unless on (cluster, ns) b`,
		`sum by (ns, source) (x) ignoring (a)`: `sum by (cluster, ns, source) (x) ignoring (a)`,
	} {
		if got := group(in, []string{"cluster", "ns"}); got != want {
			t.Errorf("group(%s) = %s, want %s", in, got, want)
		}
	}
}

// gaugeRe matches a gauge each ehlo process reports for itself. Two
// processes reporting the same source or lane must not add up.
var gaugeRe = regexp.MustCompile(`\b(ehlo_queue_[a-z_]+|ehlo_warmup_[a-z_]+|ehlo_route_last_event_timestamp_seconds)\b`)

// replicaSafeBefore matches what may come right before a gauge selector:
// a max by, a presence test, a label lookup or a set operation.
var replicaSafeBefore = regexp.MustCompile(`(max by \([^)]*\) \((max_over_time\()?|present_over_time\(|label_values\(|unless on \([^)]*\) )$`)

func TestGaugesAreTakenOncePerLane(t *testing.T) {
	check := func(where, expr string) {
		for _, loc := range gaugeRe.FindAllStringIndex(expr, -1) {
			if !replicaSafeBefore.MatchString(expr[:loc[0]]) {
				t.Errorf("%s: %s is not under max by, so two ehlo processes double it", where, expr[loc[0]:loc[1]])
			}
		}
	}
	for _, d := range Dashboards() {
		for where, expr := range renderedExprs(t, Default(), d) {
			check(where, expr)
		}
	}
	for _, r := range renderedRules(t, Default()) {
		check(r.Record+r.Alert, r.Expr)
	}
}
