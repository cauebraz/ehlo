package monitoring

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func prodConfig() Config {
	cfg := Default()
	cfg.Selector = `job="mail-ehlo"`
	cfg.GroupBy = []string{"cluster"}
	return cfg
}

func stagingConfig() Config {
	cfg := Default()
	cfg.Selector = `job="ehlo-staging"`
	cfg.GroupBy = []string{"cluster"}
	cfg.UIDPrefix = "staging-"
	return cfg
}

var byClauseRe = regexp.MustCompile(`\bby \(([^)]*)\)`)

func TestTwoRendersDoNotCollide(t *testing.T) {
	type render struct {
		cfg Config
		job string
	}
	renders := []render{{prodConfig(), `job="mail-ehlo"`}, {stagingConfig(), `job="ehlo-staging"`}}
	groupNames := map[string]string{}
	kubeNames := map[string]string{}
	for _, rd := range renders {
		for _, g := range renderedGroups(t, rd.cfg) {
			if other, ok := groupNames[g.Name]; ok {
				t.Errorf("rule group %s is in the renders for %s and %s", g.Name, other, rd.job)
			}
			groupNames[g.Name] = rd.job
			for _, r := range g.Rules {
				where := rd.job + " " + r.Record + r.Alert
				for _, m := range byClauseRe.FindAllStringSubmatch(r.Expr, -1) {
					if !slices.Contains(strings.Split(strings.ReplaceAll(m[1], " ", ""), ","), "job") {
						t.Errorf("%s: %s drops job, so the other render's series look the same", where, m[0])
					}
				}
				for _, m := range seriesSelectorRe.FindAllStringSubmatch(r.Expr, -1) {
					if !strings.Contains(m[2], rd.job) {
						t.Errorf("%s: %s does not select its own job", where, m[0])
					}
				}
			}
		}
		for _, d := range Dashboards() {
			for where, expr := range renderedExprs(t, rd.cfg, d) {
				for _, m := range seriesSelectorRe.FindAllStringSubmatch(expr, -1) {
					if !strings.Contains(m[2], rd.job) {
						t.Errorf("%s %s: %s reads another render's series", rd.job, where, m[0])
					}
				}
			}
		}
		files, err := Files(rd.cfg, FormatAll)
		if err != nil {
			t.Fatal(err)
		}
		rule := yamlDocs(t, files[PrometheusRuleFile])[0]
		mimir := yamlDocs(t, files[MimirFile])[0]
		for kind, name := range map[string]any{"PrometheusRule": dig(t, rule, "metadata", "name"), "mimir namespace": mimir["namespace"]} {
			key := kind + " " + name.(string)
			if other, ok := kubeNames[key]; ok {
				t.Errorf("%s is in the renders for %s and %s", key, other, rd.job)
			}
			kubeNames[key] = rd.job
		}
	}
}

func TestDefaultRuleNamesStay(t *testing.T) {
	var names []string
	for _, g := range renderedGroups(t, Default()) {
		names = append(names, g.Name)
	}
	if !slices.Equal(names, []string{"ehlo-recording", "ehlo-alerts"}) {
		t.Errorf("default groups = %v", names)
	}
	files, err := Files(Default(), FormatAll)
	if err != nil {
		t.Fatal(err)
	}
	if n := dig(t, yamlDocs(t, files[PrometheusRuleFile])[0], "metadata", "name"); n != "ehlo" {
		t.Errorf("default PrometheusRule name = %v", n)
	}
	if n := yamlDocs(t, files[MimirFile])[0]["namespace"]; n != "ehlo" {
		t.Errorf("default mimir namespace = %v", n)
	}
}

func TestOnlyEqualityMatchersScopeTheRender(t *testing.T) {
	cfg := Default()
	cfg.Selector = `job=~"a|b", namespace="mail", env!="dev"`
	for _, r := range renderedRules(t, cfg) {
		for _, m := range byClauseRe.FindAllStringSubmatch(r.Expr, -1) {
			if strings.Contains(m[1], "job") || strings.Contains(m[1], "env") || !strings.Contains(m[1], "namespace") {
				t.Errorf("%s%s: %s, want namespace kept and job, env not", r.Record, r.Alert, m[0])
			}
		}
		for _, m := range seriesSelectorRe.FindAllStringSubmatch(r.Expr, -1) {
			if strings.HasPrefix(m[1], "ehlo:") && (!strings.Contains(m[2], `namespace="mail"`) || strings.Contains(m[2], "job") || strings.Contains(m[2], "env")) {
				t.Errorf("%s%s: %s, want only namespace=\"mail\" on recorded series", r.Record, r.Alert, m[0])
			}
		}
	}
}

func TestDashboardURLKeepsPathAndQuery(t *testing.T) {
	cfg := Default()
	cfg.DashboardURL = "https://grafana.example.com/grafana?orgId=2"
	for _, r := range renderedRules(t, cfg) {
		if r.Alert == "" {
			continue
		}
		want := "https://grafana.example.com/grafana/d/" + r.Annotations["dashboard_uid"] + "?orgId=2"
		if got := r.Annotations["dashboard_url"]; got != want {
			t.Errorf("%s dashboard_url = %q, want %q", r.Alert, got, want)
		}
	}
}

func TestCodeLinksKeepTheFilters(t *testing.T) {
	cfg := prodConfig()
	var codes Dashboard
	for _, d := range Dashboards() {
		if d.Name == codesName {
			codes = d
		}
	}
	varNames := func(d Dashboard) []string {
		var out []string
		spec := field[obj](t, renderedDoc(t, cfg, d), "spec", d.Name)
		for _, v := range field[[]any](t, spec, "variables", d.Name) {
			out = append(out, field[string](t, field[obj](t, v.(obj), "spec", d.Name), "name", d.Name))
		}
		return out
	}
	codeVars := varNames(codes)
	found := 0
	for _, d := range Dashboards() {
		b, err := DashboardJSON(cfg, d)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(b, []byte("/d/ehlo-codes")) {
			continue
		}
		for _, v := range varNames(d) {
			if !slices.Contains(codeVars, v) {
				t.Errorf("%s has variable %s the Reply codes dashboard lacks", d.Name, v)
			}
		}
		for _, link := range regexp.MustCompile(`"url": "(/d/ehlo-codes[^"]*)"`).FindAllStringSubmatch(string(b), -1) {
			found++
			url := link[1]
			if !strings.Contains(url, "${__url_time_range}") || !strings.Contains(url, "${__all_variables}") {
				t.Errorf("%s: link %s drops the time range or the filters", d.Name, url)
			}
			if strings.Count(url, "?") != 1 {
				t.Errorf("%s: link %s is not one query string", d.Name, url)
			}
		}
		if !bytes.Contains(b, []byte("/d/ehlo-codes?viewPanel=panel-")) {
			t.Errorf("%s: no link opens one code panel", d.Name)
		}
	}
	if found == 0 {
		t.Fatal("no link to the Reply codes dashboard")
	}
}

func TestRenderRefusesStaleDashboards(t *testing.T) {
	dir := t.TempDir()
	if err := Render(Default(), FormatAll, dir); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(dir, DashboardsDir, "team-board.json")
	if err := os.WriteFile(foreign, []byte(`{"apiVersion": "dashboard.grafana.app/v2", "spec": {"tags": ["mail"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Render(Default(), FormatAll, dir); err != nil {
		t.Errorf("a second render of the same config: %v", err)
	}
	other := Default()
	other.UIDPrefix = "mail-"
	err := Render(other, FormatGrafana, dir)
	if err == nil || !strings.Contains(err.Error(), "ehlo-overview.json") {
		t.Fatalf("render over another uid_prefix = %v, want an error naming the stale files", err)
	}
	if err := Render(other, FormatPrometheus, dir); err != nil {
		t.Errorf("a format without dashboards: %v", err)
	}
	removed, err := Clean(other, FormatAll, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != len(Dashboards()) {
		t.Errorf("Clean removed %v, want the %d ehlo- dashboards", removed, len(Dashboards()))
	}
	if err := Render(other, FormatAll, dir); err != nil {
		t.Fatal(err)
	}
	left, err := filepath.Glob(filepath.Join(dir, DashboardsDir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != len(Dashboards())+1 || !slices.Contains(left, foreign) {
		t.Errorf("dashboards after Clean = %v, want the mail- render and the foreign file", left)
	}
	for _, f := range left {
		if strings.Contains(filepath.Base(f), "ehlo-") {
			t.Errorf("%s survived Clean", f)
		}
	}
}

func TestGrafanaManifestNeedsAnInstanceSelector(t *testing.T) {
	cfg := Default()
	cfg.Kubernetes.InstanceSelector = map[string]string{}
	for _, f := range []string{FormatGrafanaManifest, FormatAll} {
		if _, err := Files(cfg, f); err == nil {
			t.Errorf("%s renders with an empty instance_selector, which selects every Grafana", f)
		}
	}
	if _, err := Files(cfg, FormatConfigMap); err != nil {
		t.Errorf("configmap needs no instance_selector: %v", err)
	}
}
