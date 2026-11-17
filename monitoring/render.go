package monitoring

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Formats Render accepts. FormatAll renders every other one.
const (
	FormatAll             = "all"
	FormatGrafana         = "grafana"
	FormatPrometheus      = "prometheus"
	FormatMimir           = "mimir"
	FormatPrometheusRule  = "prometheusrule"
	FormatConfigMap       = "configmap"
	FormatGrafanaManifest = "grafanamanifest"
)

// Formats lists every format but FormatAll, in render order.
var Formats = []string{FormatGrafana, FormatPrometheus, FormatMimir, FormatPrometheusRule, FormatConfigMap, FormatGrafanaManifest}

// Output paths under the render directory.
const (
	DashboardsDir      = "dashboards"
	RulesDir           = "rules"
	MimirFile          = "mimir/ehlo.yaml"
	PrometheusRuleFile = "kubernetes/prometheusrule.yaml"
	ConfigMapsFile     = "kubernetes/dashboards-configmaps.yaml"
	GrafanaManifests   = "kubernetes/grafana-manifests.yaml"
)

// Render writes the files of format for cfg under outDir and creates the
// directories they need. It fails, before writing anything, when the
// dashboards directory holds an ehlo dashboard the render would not write,
// such as one of an earlier uid_prefix; Clean removes them.
func Render(cfg Config, format, outDir string) error {
	files, err := Files(cfg, format)
	if err != nil {
		return err
	}
	stale, err := staleFiles(files, format, outDir)
	if err != nil {
		return err
	}
	if len(stale) > 0 {
		return fmt.Errorf("%s holds ehlo dashboards this render does not write, such as those of another uid_prefix: %s; remove them or render with -clean (Clean)",
			filepath.Join(outDir, DashboardsDir), strings.Join(stale, ", "))
	}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		path := filepath.Join(outDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(path, files[name], 0o644); err != nil { // #nosec G306 -- dashboards and rules are not secret
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	return nil
}

// Clean removes the ehlo dashboards under outDir that a render of format
// for cfg would not write, and returns their paths. A dashboard is ehlo's
// when it is a schema v2 resource tagged ehlo; other files stay.
func Clean(cfg Config, format, outDir string) ([]string, error) {
	files, err := Files(cfg, format)
	if err != nil {
		return nil, err
	}
	stale, err := staleFiles(files, format, outDir)
	if err != nil {
		return nil, err
	}
	for _, path := range stale {
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	return stale, nil
}

// staleFiles returns the ehlo dashboards in the dashboards directory under
// outDir that are not among files.
func staleFiles(files map[string][]byte, format, outDir string) ([]string, error) {
	if format != FormatAll && format != FormatGrafana {
		return nil, nil
	}
	paths, err := filepath.Glob(filepath.Join(outDir, DashboardsDir, "*.json"))
	if err != nil {
		return nil, err
	}
	var stale []string
	for _, path := range paths {
		if _, ok := files[DashboardsDir+"/"+filepath.Base(path)]; ok {
			continue
		}
		b, err := os.ReadFile(path) // #nosec G304 -- path is a dashboard under the operator's output directory
		if err != nil {
			return nil, err
		}
		var d struct {
			APIVersion string `json:"apiVersion"`
			Spec       struct {
				Tags []string `json:"tags"`
			} `json:"spec"`
		}
		if json.Unmarshal(b, &d) == nil && d.APIVersion == "dashboard.grafana.app/v2" && slices.Contains(d.Spec.Tags, "ehlo") {
			stale = append(stale, path)
		}
	}
	return stale, nil
}

// Files returns the files of format for cfg, keyed by slash-separated path
// relative to the render directory.
func Files(cfg Config, format string) (map[string][]byte, error) {
	cfg = cfg.withDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	formats := []string{format}
	if format == FormatAll {
		formats = Formats
	} else if !slices.Contains(Formats, format) {
		return nil, fmt.Errorf("format %q: want %s or %s", format, strings.Join(Formats, ", "), FormatAll)
	}
	if slices.Contains(formats, FormatGrafanaManifest) && len(cfg.Kubernetes.InstanceSelector) == 0 {
		return nil, fmt.Errorf("%s: kubernetes.instance_selector is empty, which selects every Grafana instance", FormatGrafanaManifest)
	}
	r := newRenderer(cfg)
	out := map[string][]byte{}
	for _, f := range formats {
		if err := r.files(f, out); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
	}
	return out, nil
}

func (r renderer) files(format string, out map[string][]byte) error {
	switch format {
	case FormatGrafana:
		for _, d := range Dashboards() {
			b, err := r.json(d)
			if err != nil {
				return err
			}
			out[DashboardsDir+"/"+r.uid(d.Name)+".json"] = b
		}
	case FormatPrometheus:
		names := []string{RecordingFile, AlertsFile}
		for i, g := range r.ruleGroups(RuleGroups()) {
			b, err := marshalYAML(ordered{kv{"groups", []any{g}}})
			if err != nil {
				return err
			}
			out[RulesDir+"/"+names[i]] = b
		}
	case FormatMimir:
		b, err := marshalYAML(ordered{kv{"namespace", r.ruleNamespace()}, kv{"groups", r.ruleGroups(RuleGroups())}})
		if err != nil {
			return err
		}
		out[MimirFile] = b
	case FormatPrometheusRule:
		b, err := marshalYAML(ordered{
			kv{"apiVersion", "monitoring.coreos.com/v1"},
			kv{"kind", "PrometheusRule"},
			kv{"metadata", r.metadata(r.ruleNamespace(), nil)},
			kv{"spec", ordered{kv{"groups", r.ruleGroups(RuleGroups())}}},
		})
		if err != nil {
			return err
		}
		out[PrometheusRuleFile] = b
	case FormatConfigMap:
		var docs []any
		for _, d := range Dashboards() {
			b, err := r.json(d)
			if err != nil {
				return err
			}
			docs = append(docs, ordered{
				kv{"apiVersion", "v1"},
				kv{"kind", "ConfigMap"},
				kv{"metadata", r.metadata(r.uid(d.Name)+"-dashboard", r.cfg.ConfigMapLabel)},
				kv{"data", ordered{kv{r.uid(d.Name) + ".json", string(b)}}},
			})
		}
		b, err := marshalYAML(docs...)
		if err != nil {
			return err
		}
		out[ConfigMapsFile] = b
	case FormatGrafanaManifest:
		var docs []any
		for _, d := range Dashboards() {
			b, err := r.json(d)
			if err != nil {
				return err
			}
			v, err := jsonValue(b)
			if err != nil {
				return err
			}
			res := v.(map[string]any)
			meta := ordered{kv{"name", r.uid(d.Name)}}
			if r.cfg.FolderUID != "" {
				meta = append(meta, kv{"annotations", map[string]string{"grafana.app/folder": r.cfg.FolderUID}})
			}
			docs = append(docs, ordered{
				kv{"apiVersion", "grafana.integreatly.org/v1beta1"},
				kv{"kind", "GrafanaManifest"},
				kv{"metadata", r.metadata(r.uid(d.Name), nil)},
				kv{"spec", ordered{
					kv{"instanceSelector", ordered{kv{"matchLabels", r.cfg.Kubernetes.InstanceSelector}}},
					kv{"template", ordered{
						kv{"apiVersion", res["apiVersion"]},
						kv{"kind", res["kind"]},
						kv{"metadata", meta},
						kv{"spec", res["spec"]},
					}},
				}},
			})
		}
		b, err := marshalYAML(docs...)
		if err != nil {
			return err
		}
		out[GrafanaManifests] = b
	}
	return nil
}

// metadata is the metadata of a Kubernetes object: its name, the
// configured namespace, labels and annotations, and extra labels.
func (r renderer) metadata(name string, extra map[string]string) ordered {
	m := ordered{kv{"name", name}}
	if ns := r.cfg.Kubernetes.Namespace; ns != "" {
		m = append(m, kv{"namespace", ns})
	}
	labels := map[string]string{}
	for k, v := range r.cfg.Kubernetes.Labels {
		labels[k] = v
	}
	for k, v := range extra {
		labels[k] = v
	}
	if len(labels) > 0 {
		m = append(m, kv{"labels", labels})
	}
	if len(r.cfg.Kubernetes.Annotations) > 0 {
		m = append(m, kv{"annotations", r.cfg.Kubernetes.Annotations})
	}
	return m
}
