// Command ehlo-monitoring renders ehlo's Grafana dashboards and Prometheus
// rules for a monitoring stack, and keeps the committed default render in
// grafana/dashboards and prometheus/rules up to date.
//
//	ehlo-monitoring render [-config monitoring.yaml] [flags] [-clean] -format all -out DIR
//	ehlo-monitoring check  [-root DIR]
//	ehlo-monitoring update [-root DIR]
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/cauebraz/ehlo/monitoring"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "ehlo-monitoring:", err)
		}
		os.Exit(1)
	}
}

const usage = `usage:
  ehlo-monitoring render [-config FILE] [flags] [-clean] -format FORMAT -out DIR
  ehlo-monitoring check  [-root DIR]
  ehlo-monitoring update [-root DIR]

render writes the dashboards and rules for your stack. FORMAT is all, grafana, prometheus, mimir, prometheusrule, configmap or grafanamanifest.
render refuses to leave ehlo dashboards it did not write in DIR/dashboards, such as those of an earlier uid_prefix; -clean removes them.
check fails when grafana/dashboards or prometheus/rules differ from the default render.
update writes the default render to grafana/dashboards and prometheus/rules.
`

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return errors.New("missing command")
	}
	switch args[0] {
	case "render":
		return render(args[1:], stdout, stderr)
	case "check", "update":
		fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
		fs.SetOutput(stderr)
		root := fs.String("root", ".", "repository root")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return committed(*root, args[0] == "check")
	case "-h", "-help", "--help", "help":
		_, err := fmt.Fprint(stdout, usage)
		return err
	}
	_, _ = fmt.Fprint(stderr, usage)
	return fmt.Errorf("unknown command %q", args[0])
}

// mapFlag is a k=v,k=v flag.
type mapFlag struct{ m *map[string]string }

func (f mapFlag) String() string {
	if f.m == nil || *f.m == nil {
		return ""
	}
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(*f.m)) {
		parts = append(parts, k+"="+(*f.m)[k])
	}
	return strings.Join(parts, ",")
}

func (f mapFlag) Set(s string) error {
	m := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok || k == "" {
			return fmt.Errorf("%q is not key=value", part)
		}
		m[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	*f.m = m
	return nil
}

// listFlag is a comma-separated list flag.
type listFlag struct{ l *[]string }

func (f listFlag) String() string {
	if f.l == nil {
		return ""
	}
	return strings.Join(*f.l, ",")
}

func (f listFlag) Set(s string) error {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	*f.l = out
	return nil
}

func render(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("render", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "YAML file with the keys below; flags override it")
	format := fs.String("format", monitoring.FormatAll, "all, "+strings.Join(monitoring.Formats, ", "))
	out := fs.String("out", "", "output directory")
	clean := fs.Bool("clean", false, "remove ehlo dashboards in DIR/dashboards that this render does not write, such as those of an earlier uid_prefix")
	var c monitoring.Config
	fs.StringVar(&c.Selector, "selector", "", `selector: label matchers added to every ehlo_ selector, such as job="mail-ehlo"`)
	fs.Var(listFlag{&c.GroupBy}, "group-by", "group_by: labels every aggregation keeps, such as cluster,namespace")
	fs.StringVar(&c.Datasource, "datasource", "", "datasource: uid the datasource variable selects first; empty selects the default datasource")
	fs.StringVar(&c.UIDPrefix, "uid-prefix", "", "uid_prefix: dashboard uid prefix (default "+monitoring.DefaultUIDPrefix+")")
	fs.StringVar(&c.AlertsMetric, "alerts-metric", "", "alerts_metric: ALERTS or GRAFANA_ALERTS (default "+monitoring.DefaultAlertsMetric+")")
	fs.StringVar(&c.RuleInterval, "rule-interval", "", "rule_interval: rule group evaluation interval (default "+monitoring.DefaultRuleInterval+")")
	fs.Var(mapFlag{&c.RuleLabels}, "rule-labels", "rule_labels: extra alert labels, such as team=mail")
	fs.StringVar(&c.RunbookURL, "runbook-url", "", "runbook_url: pattern, {{alert}} becomes the lowercased alert name (default "+monitoring.DefaultRunbookURL+")")
	fs.StringVar(&c.DashboardURL, "dashboard-url", "", "dashboard_url: Grafana base URL for the dashboard_url annotation")
	fs.StringVar(&c.FolderUID, "folder-uid", "", "folder_uid: Grafana folder uid of the GrafanaManifest objects")
	fs.StringVar(&c.Kubernetes.Namespace, "namespace", "", "kubernetes.namespace: namespace of the Kubernetes objects")
	fs.Var(mapFlag{&c.Kubernetes.Labels}, "labels", "kubernetes.labels: labels of the Kubernetes objects, such as release=kube-prometheus-stack")
	fs.Var(mapFlag{&c.Kubernetes.Annotations}, "annotations", "kubernetes.annotations: annotations of the Kubernetes objects")
	fs.Var(mapFlag{&c.Kubernetes.InstanceSelector}, "instance-selector", "kubernetes.instance_selector: Grafana instance labels of the GrafanaManifest objects (default dashboards=grafana)")
	fs.Var(mapFlag{&c.ConfigMapLabel}, "configmap-label", "configmap_label: label the Grafana sidecar watches (default grafana_dashboard=1)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("render: unexpected argument %q", fs.Arg(0))
	}
	if *out == "" {
		return errors.New("render: -out is required")
	}
	cfg := monitoring.Default()
	if *configPath != "" {
		var err error
		if cfg, err = monitoring.LoadConfig(*configPath); err != nil {
			return err
		}
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "selector":
			cfg.Selector = c.Selector
		case "group-by":
			cfg.GroupBy = c.GroupBy
		case "datasource":
			cfg.Datasource = c.Datasource
		case "uid-prefix":
			cfg.UIDPrefix = c.UIDPrefix
		case "alerts-metric":
			cfg.AlertsMetric = c.AlertsMetric
		case "rule-interval":
			cfg.RuleInterval = c.RuleInterval
		case "rule-labels":
			cfg.RuleLabels = c.RuleLabels
		case "runbook-url":
			cfg.RunbookURL = c.RunbookURL
		case "dashboard-url":
			cfg.DashboardURL = c.DashboardURL
		case "folder-uid":
			cfg.FolderUID = c.FolderUID
		case "namespace":
			cfg.Kubernetes.Namespace = c.Kubernetes.Namespace
		case "labels":
			cfg.Kubernetes.Labels = c.Kubernetes.Labels
		case "annotations":
			cfg.Kubernetes.Annotations = c.Kubernetes.Annotations
		case "instance-selector":
			cfg.Kubernetes.InstanceSelector = c.Kubernetes.InstanceSelector
		case "configmap-label":
			cfg.ConfigMapLabel = c.ConfigMapLabel
		}
	})
	files, err := monitoring.Files(cfg, *format)
	if err != nil {
		return err
	}
	if *clean {
		removed, err := monitoring.Clean(cfg, *format, *out)
		if err != nil {
			return err
		}
		for _, path := range removed {
			if _, err := fmt.Fprintln(stdout, "removed", path); err != nil {
				return err
			}
		}
	}
	if err := monitoring.Render(cfg, *format, *out); err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if _, err := fmt.Fprintln(stdout, filepath.Join(*out, filepath.FromSlash(name))); err != nil {
			return err
		}
	}
	return nil
}

// Committed paths of the default render, relative to the repository root.
const (
	dashboardsDir = "grafana/dashboards"
	rulesDir      = "prometheus/rules"
)

// committed checks or writes the default render in the repository. Every
// JSON file in grafana/dashboards must come from the render.
func committed(root string, check bool) error {
	cfg := monitoring.Default()
	want := map[string][]byte{}
	for _, format := range []string{monitoring.FormatGrafana, monitoring.FormatPrometheus} {
		files, err := monitoring.Files(cfg, format)
		if err != nil {
			return err
		}
		for name, b := range files {
			switch dir, file := filepath.Split(filepath.FromSlash(name)); filepath.Clean(dir) {
			case monitoring.DashboardsDir:
				want[filepath.Join(root, dashboardsDir, file)] = b
			case monitoring.RulesDir:
				want[filepath.Join(root, rulesDir, file)] = b
			}
		}
	}
	existing, err := filepath.Glob(filepath.Join(root, dashboardsDir, "*.json"))
	if err != nil {
		return err
	}
	for _, path := range existing {
		if _, ok := want[path]; ok {
			continue
		}
		if check {
			return fmt.Errorf("%s is not generated by the monitoring package, run ehlo-monitoring update", path)
		}
		if err := os.Remove(path); err != nil { // #nosec G703 -- path comes from a glob under the repository root
			return err
		}
	}
	for _, path := range slices.Sorted(maps.Keys(want)) {
		b := want[path]
		if check {
			got, err := os.ReadFile(path) // #nosec G304 -- path is under the repository root
			if err != nil || !bytes.Equal(got, b) {
				return fmt.Errorf("%s is stale, run ehlo-monitoring update", path)
			}
			continue
		}
		if err := os.WriteFile(path, b, 0o644); err != nil { // #nosec G306 -- dashboards and rules are not secret
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	return nil
}
