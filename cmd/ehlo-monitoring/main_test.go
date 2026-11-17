package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repo makes a root with the two committed directories.
func repo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{dashboardsDir, rulesDir} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func cli(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := run(args, &stdout, &stderr)
	return stdout.String() + stderr.String(), err
}

func TestUpdateThenCheckPasses(t *testing.T) {
	root := repo(t)
	if _, err := cli(t, "update", "-root", root); err != nil {
		t.Fatalf("update: %v", err)
	}
	for _, f := range []string{"grafana/dashboards/ehlo-overview.json", "prometheus/rules/ehlo-recording.yml", "prometheus/rules/ehlo-alerts.yml"} {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			t.Errorf("update did not write %s: %v", f, err)
		}
	}
	if _, err := cli(t, "check", "-root", root); err != nil {
		t.Fatalf("check after update: %v", err)
	}
}

func TestCheckReportsStaleFiles(t *testing.T) {
	for _, file := range []string{"grafana/dashboards/ehlo-overview.json", "prometheus/rules/ehlo-alerts.yml"} {
		root := repo(t)
		if _, err := cli(t, "update", "-root", root); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, file)
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := cli(t, "check", "-root", root)
		if err == nil || !strings.Contains(err.Error(), path+" is stale") {
			t.Errorf("check = %v, want %s is stale", err, path)
		}
	}
}

func TestCheckReportsStrayDashboard(t *testing.T) {
	root := repo(t)
	if _, err := cli(t, "update", "-root", root); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(root, dashboardsDir, "ehlo-handmade.json")
	if err := os.WriteFile(stray, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := cli(t, "check", "-root", root)
	if err == nil || !strings.Contains(err.Error(), stray+" is not generated") {
		t.Fatalf("check = %v, want %s is not generated", err, stray)
	}
	if _, err := cli(t, "update", "-root", root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Fatalf("update kept %s, stat err = %v", stray, err)
	}
}

func TestRenderFlagsOverrideTheConfigFile(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "monitoring.yaml")
	if err := os.WriteFile(config, []byte("selector: job=\"from-file\"\nuid_prefix: file-\nrule_labels: {team: mail}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	printed, err := cli(t, "render", "-config", config, "-selector", `job="mail-ehlo"`, "-group-by", "cluster", "-format", "prometheus", "-out", out)
	if err != nil {
		t.Fatalf("render: %v\n%s", err, printed)
	}
	if !strings.Contains(printed, filepath.Join(out, "rules", "ehlo-alerts.yml")) {
		t.Errorf("render printed %q, want the files it wrote", printed)
	}
	b, err := os.ReadFile(filepath.Join(out, "rules", "ehlo-alerts.yml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{`job="mail-ehlo"`, "by (cluster,", "team: mail", "dashboard_uid: file-"} {
		if !strings.Contains(s, want) {
			t.Errorf("alerts lack %q", want)
		}
	}
	if strings.Contains(s, "from-file") {
		t.Error("the -selector flag did not override the config file")
	}
}

func TestRenderEveryFormat(t *testing.T) {
	out := t.TempDir()
	if _, err := cli(t, "render", "-format", "all", "-out", out, "-namespace", "monitoring", "-labels", "release=kube-prometheus-stack"); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"dashboards/ehlo-overview.json", "rules/ehlo-recording.yml", "mimir/ehlo.yaml", "kubernetes/prometheusrule.yaml", "kubernetes/dashboards-configmaps.yaml", "kubernetes/grafana-manifests.yaml"} {
		b, err := os.ReadFile(filepath.Join(out, f))
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		if strings.HasPrefix(f, "kubernetes/") && !bytes.Contains(b, []byte("release: kube-prometheus-stack")) {
			t.Errorf("%s lacks the release label", f)
		}
	}
}

func TestRenderRejectsBadInput(t *testing.T) {
	for _, args := range [][]string{
		{"render"},
		{"render", "-out", t.TempDir(), "-format", "classic"},
		{"render", "-out", t.TempDir(), "-alerts-metric", "alerts"},
		{"render", "-out", t.TempDir(), "-labels", "release"},
		{"render", "-out", t.TempDir(), "extra"},
		{"render", "-out", t.TempDir(), "-group-by", "class"},
		{"render", "-out", t.TempDir(), "-namespace", "Bad_NS"},
		{"render", "-out", t.TempDir(), "-instance-selector", ""},
		{"draw"},
		{},
	} {
		if _, err := cli(t, args...); err == nil {
			t.Errorf("%q passed", args)
		}
	}
}

func TestRenderCleanRemovesAnEarlierUIDPrefix(t *testing.T) {
	out := t.TempDir()
	if _, err := cli(t, "render", "-format", "grafana", "-out", out); err != nil {
		t.Fatal(err)
	}
	if printed, err := cli(t, "render", "-format", "grafana", "-uid-prefix", "mail-", "-out", out); err == nil || !strings.Contains(printed+err.Error(), "-clean") {
		t.Fatalf("render over an earlier uid_prefix = %v, want an error that names -clean", err)
	}
	printed, err := cli(t, "render", "-format", "grafana", "-uid-prefix", "mail-", "-clean", "-out", out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(printed, "removed "+filepath.Join(out, "dashboards", "ehlo-overview.json")) {
		t.Errorf("render -clean printed %q, want the files it removed", printed)
	}
	if _, err := os.Stat(filepath.Join(out, "dashboards", "ehlo-overview.json")); !os.IsNotExist(err) {
		t.Errorf("ehlo-overview.json survived -clean: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "dashboards", "mail-overview.json")); err != nil {
		t.Error(err)
	}
}
