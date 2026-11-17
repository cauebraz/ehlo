package monitoring

import (
	"bytes"
	"cmp"
	"encoding/json"
	"net/url"
	"slices"
	"strings"
)

// renderer renders the dashboards and rules for one Config.
type renderer struct {
	cfg Config
	// dash holds the matchers of dashboard queries; rule those of rules.
	dash, rule matchers
}

func newRenderer(cfg Config) renderer {
	sel := selectorMatchers(cfg.Selector)
	scope := scopeMatchers(cfg.Selector)
	vars := variableMatchers(cfg.GroupBy)
	own := append(slices.Clone(vars), scope...)
	return renderer{
		cfg:  cfg,
		dash: matchers{raw: append(slices.Clone(sel), vars...), recorded: own, alerts: own, alertsMetric: cfg.AlertsMetric},
		rule: matchers{raw: sel, recorded: scope},
	}
}

// uid is the Grafana uid of the dashboard named name.
func (r renderer) uid(name string) string { return r.cfg.UIDPrefix + name }

// dashboardPath is the path of the dashboard named name, without a query
// string.
func (r renderer) dashboardPath(name string) string { return "/d/" + r.uid(name) }

// dashboardURL is the absolute URL of the dashboard named name under
// DashboardURL, which keeps its own path and query string.
func (r renderer) dashboardURL(name string) string {
	u, err := url.Parse(r.cfg.DashboardURL)
	if err != nil {
		return ""
	}
	return u.JoinPath("d", r.uid(name)).String()
}

// ruleNamespace names the rule groups, the Mimir namespace and the
// PrometheusRule: the uid prefix without its trailing hyphens.
func (r renderer) ruleNamespace() string { return strings.TrimRight(r.cfg.UIDPrefix, "-") }

// dashboardExpr applies the selector, the group_by labels and the alert
// metric to a dashboard query.
func (r renderer) dashboardExpr(expr string) string {
	return group(r.dash.inject(expr), r.cfg.GroupBy)
}

// legend puts the group_by labels in front of a legend, so series of two
// clusters stay apart.
func (r renderer) legend(l string) string {
	if l == "" || len(r.cfg.GroupBy) == 0 {
		return l
	}
	parts := make([]string, len(r.cfg.GroupBy))
	for i, g := range r.cfg.GroupBy {
		parts[i] = "{{" + g + "}}"
	}
	return strings.Join(parts, " ") + " " + l
}

// DashboardJSON returns one dashboard as the indented JSON of its Grafana
// schema v2 resource.
func DashboardJSON(cfg Config, d Dashboard) ([]byte, error) {
	cfg = cfg.withDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return newRenderer(cfg).json(d)
}

func (r renderer) json(d Dashboard) ([]byte, error) {
	return marshalJSON(r.dashboard(d))
}

func marshalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// dashboard returns the Grafana schema v2 resource of one dashboard.
func (r renderer) dashboard(d Dashboard) obj {
	elements := obj{}
	rows := make([]any, len(d.Rows))
	id := 1
	for k, row := range d.Rows {
		grids := make([]any, len(row.Groups))
		for g, group := range row.Groups {
			var items []any
			for _, p := range group.Panels {
				pid := p.ID
				if pid == 0 {
					pid = id
					id++
				}
				elements[elementName(pid)] = r.element(p, pid)
				items = append(items, autoGridItem(pid, p))
			}
			grids[g] = obj{"kind": "AutoGridLayout", "spec": obj{
				"maxColumnCount":  group.Columns,
				"columnWidthMode": "custom",
				"columnWidth":     group.Width,
				"rowHeightMode":   group.Height,
				"fillScreen":      false,
				"items":           items,
			}}
		}
		layout := grids[0]
		if len(grids) > 1 {
			sub := make([]any, len(grids))
			for g, grid := range grids {
				sub[g] = obj{"kind": "RowsLayoutRow", "spec": obj{"hideHeader": true, "collapse": false, "layout": grid}}
			}
			layout = obj{"kind": "RowsLayout", "spec": obj{"rows": sub}}
		}
		rows[k] = obj{"kind": "RowsLayoutRow", "spec": obj{"title": row.Title, "collapse": row.Collapsed, "layout": layout}}
	}
	return obj{
		"apiVersion": "dashboard.grafana.app/v2",
		"kind":       "Dashboard",
		"metadata":   obj{"name": r.uid(d.Name)},
		"spec": obj{
			"title":       "ehlo / " + d.Title,
			"description": d.Description,
			"tags":        []any{"smtp", "email", "ehlo"},
			"editable":    false,
			"cursorSync":  "Crosshair",
			"preload":     false,
			"timeSettings": obj{
				"from": cmp.Or(d.Range, "now-3h"), "to": "now", "timezone": "utc",
				"autoRefresh": "30s", "autoRefreshIntervals": []any{"30s", "1m", "5m", "15m", "1h"},
				"hideTimepicker": false, "fiscalYearStartMonth": 0,
			},
			"links": []any{obj{
				"title": "Other dashboards", "type": "dashboards", "icon": "external link", "tooltip": "",
				"tags": []any{"ehlo"}, "asDropdown": true, "includeVars": true, "keepTime": true, "targetBlank": false,
			}},
			"annotations": r.annotations(),
			"variables":   r.variables(d),
			"elements":    elements,
			"layout":      obj{"kind": "RowsLayout", "spec": obj{"rows": rows}},
		},
	}
}

// autoGridItem places element id in an auto grid. A panel that hides
// while empty carries the condition that shows it only with data.
func autoGridItem(id int, p Panel) obj {
	spec := obj{"element": obj{"kind": "ElementReference", "name": elementName(id)}}
	if p.HideEmpty {
		spec["conditionalRendering"] = obj{"kind": "ConditionalRenderingGroup", "spec": obj{
			"visibility": "show", "condition": "and",
			"items": []any{obj{"kind": "ConditionalRenderingData", "spec": obj{"value": true}}},
		}}
	}
	return obj{"kind": "AutoGridLayoutItem", "spec": spec}
}

// variables are the data source, one filter per group_by label, the
// source filter and, unless the dashboard is fixed to one provider, the
// provider filter.
func (r renderer) variables(d Dashboard) []any {
	variable := func(name, label, query string) obj {
		return obj{"kind": "QueryVariable", "spec": obj{
			"name": name, "label": label, "hide": "dontHide", "skipUrlSync": false,
			"query": obj{
				"kind": "DataQuery", "group": "prometheus", "version": "v0",
				"datasource": obj{"name": DatasourceVar},
				"spec":       obj{"query": query, "refId": "PrometheusVariableQueryEditor-VariableQuery", "qryType": 1},
			},
			"definition": query, "refresh": "onTimeRangeChanged", "regex": "", "sort": "naturalAsc",
			"multi": true, "includeAll": true, "allValue": ".*", "allowCustomValue": true, "options": []any{},
			"current": obj{"text": []any{"All"}, "value": []any{"$__all"}},
		}}
	}
	vars := []any{
		obj{"kind": "DatasourceVariable", "spec": obj{
			"name": "datasource", "label": "Data source", "pluginId": "prometheus", "refresh": "onDashboardLoad",
			"regex": "", "hide": "dontHide", "skipUrlSync": false, "multi": false, "includeAll": false,
			"allowCustomValue": true, "options": []any{},
			"current": r.datasourceCurrent(),
		}},
	}
	sel := selectorMatchers(r.cfg.Selector)
	for i, g := range r.cfg.GroupBy {
		m := matchers{raw: append(slices.Clone(sel), variableMatchers(r.cfg.GroupBy[:i])...)}
		vars = append(vars, variable(g, g, `label_values(`+m.inject(`ehlo_build_info`)+`, `+g+`)`))
	}
	vars = append(vars, variable("source", "Server", `label_values(`+r.dash.inject(`ehlo_smtp_attempts_total{`+self+`}`)+`, source)`))
	if d.Provider == "" {
		vars = append(vars, variable("provider", "Provider", `label_values(`+r.dash.inject(`ehlo_smtp_attempts_total`)+`, provider)`))
	}
	return vars
}

// datasourceCurrent is the datasource variable's first value: the
// configured uid, or the "default" option, which Grafana resolves to its
// default datasource. An empty value would select the alphabetically first
// Prometheus datasource instead.
func (r renderer) datasourceCurrent() obj {
	if r.cfg.Datasource == "" {
		return obj{"text": "default", "value": "default"}
	}
	return obj{"text": r.cfg.Datasource, "value": r.cfg.Datasource}
}

// annotations marks firing alerts on every time series. The Prometheus
// fields stay in legacyOptions, the shape Grafana itself converts them to.
func (r renderer) annotations() []any {
	return []any{obj{"kind": "AnnotationQuery", "spec": obj{
		"name": "Alerts", "enable": true, "hide": false, "iconColor": "red",
		"query": obj{
			"kind": "DataQuery", "group": "prometheus", "version": "v0",
			"datasource": obj{"name": DatasourceVar}, "spec": obj{},
		},
		"legacyOptions": obj{
			"expr": r.dashboardExpr(alertsName + `{` + ehloAlerts + `, alertstate="firing", severity!="info"}`), "step": "15s",
			"titleFormat": "{{alertname}}", "textFormat": "{{source}} {{provider}} {{code}}", "useValueForTime": false,
		},
	}}}
}
