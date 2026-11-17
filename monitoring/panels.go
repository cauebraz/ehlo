package monitoring

import (
	"cmp"
	"regexp"
	"strconv"
)

// leadingSpace is the indentation Go source adds to a multi-line query.
var leadingSpace = regexp.MustCompile(`\n[ \t]+`)

type obj = map[string]any

// DatasourceVar is the Grafana variable every query reads its Prometheus
// data source from.
const DatasourceVar = "${datasource}"

// pluginVersion is the Grafana release whose panel option format the
// elements use, so Grafana runs no panel migration on them.
const pluginVersion = "13.3.1"

// Query is one PromQL target of a panel. Instant queries return one value
// at the end of the range instead of a series.
type Query struct {
	Expr    string
	Legend  string
	Instant bool
}

// Panel is one Grafana panel before layout. Build assigns its id and
// element name, and element renders it.
type Panel struct {
	Type  string
	Title string
	// Subtitle is the one line shown under the title. Description is the
	// full text behind the info icon.
	Subtitle        string
	Description     string
	Unit            string
	Min, Max        *float64
	Decimals        *int
	Queries         []Query
	Thresholds      []Step
	Mappings        []Mapping
	Stack           string
	LogScale        bool
	ThresholdUI     string
	Options         obj
	Transformations []Transform
	// Link is the Name of the dashboard a click on the panel opens.
	Link string
	// CodeLinks gives every series a data link to the Reply codes
	// dashboard, and each catalogued code a link to its own panel there.
	CodeLinks bool
	// ID fixes the panel id so other dashboards can link to it. 0 means
	// the next free id.
	ID int
	// NoValue is the text a stat shows when its query returns nothing.
	// Empty means "No data".
	NoValue string
	// Neutral draws a stat's value in the text color: a count that is
	// neither good nor bad.
	Neutral bool
	// HideEmpty hides the panel while its query returns nothing.
	HideEmpty bool
}

// Transform is one Grafana transformation and its options.
type Transform struct {
	ID      string
	Options obj
}

// Step is one threshold step: the color used from Value upward. A nil Value
// is the base step.
type Step struct {
	Color string
	Value *float64
}

// Mapping maps one integer value to a text and a color, for state panels.
type Mapping struct {
	Value int
	Text  string
	Color string
}

type dataLink struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

type fieldOverride struct {
	Matcher    matcher        `json:"matcher"`
	Properties []linkProperty `json:"properties"`
}

type matcher struct {
	ID      string `json:"id"`
	Options string `json:"options"`
}

type linkProperty struct {
	ID    string     `json:"id"`
	Value []dataLink `json:"value"`
}

type thresholdStep struct {
	Color string   `json:"color"`
	Value *float64 `json:"value"`
}

func ptr[T any](v T) *T { return &v }

// steps builds a threshold list from the base color and the steps above it.
func steps(base string, s ...Step) []Step {
	return append([]Step{{Color: base}}, s...)
}

// at is the threshold step that switches to color from v upward.
func at(v float64, color string) Step {
	return Step{Color: color, Value: ptr(v)}
}

// elementName is the key of panel id in the dashboard's elements, the
// same key Grafana gives the panel on screen.
func elementName(id int) string { return "panel-" + strconv.Itoa(id) }

// element renders the panel as a schema v2 Panel element.
func (r renderer) element(p Panel, id int) obj {
	overrides := []fieldOverride{}
	if p.CodeLinks {
		overrides = r.codeLinks()
	}
	transformations := make([]any, len(p.Transformations))
	for k, t := range p.Transformations {
		transformations[k] = obj{"kind": "Transformation", "group": t.ID, "spec": obj{"options": t.Options}}
	}
	spec := obj{
		"id":          id,
		"title":       p.Title,
		"description": p.Description,
		"links":       []any{},
		"data": obj{"kind": "QueryGroup", "spec": obj{
			"queries":         r.queries(p),
			"transformations": transformations,
			"queryOptions":    obj{},
		}},
		"vizConfig": obj{"kind": "VizConfig", "group": p.Type, "version": pluginVersion, "spec": obj{
			"options":     p.panelOptions(),
			"fieldConfig": obj{"defaults": r.fieldDefaults(p), "overrides": overrides},
		}},
	}
	if p.Subtitle != "" {
		spec["subtitle"] = p.Subtitle
	}
	return obj{"kind": "Panel", "spec": spec}
}

func (r renderer) queries(p Panel) []any {
	queries := make([]any, len(p.Queries))
	for k, q := range p.Queries {
		queries[k] = obj{"kind": "PanelQuery", "spec": obj{
			"refId":  string(rune('A' + k)),
			"hidden": false,
			"query": obj{
				"kind": "DataQuery", "group": "prometheus", "version": "v0",
				"datasource": obj{"name": DatasourceVar},
				"spec": obj{
					"expr":         r.dashboardExpr(leadingSpace.ReplaceAllString(q.Expr, "\n")),
					"legendFormat": r.legend(q.Legend),
					"range":        !q.Instant,
					"instant":      q.Instant,
				},
			},
		}}
	}
	return queries
}

func (r renderer) fieldDefaults(p Panel) obj {
	defaults := obj{
		"unit":  p.Unit,
		"color": obj{"mode": "palette-classic"},
	}
	if len(p.Thresholds) > 0 {
		ss := make([]thresholdStep, len(p.Thresholds))
		for k, s := range p.Thresholds {
			ss[k] = thresholdStep(s)
		}
		defaults["thresholds"] = obj{"mode": "absolute", "steps": ss}
		if p.Type == "stat" || p.Type == "bargauge" {
			defaults["color"] = obj{"mode": "thresholds"}
		}
	}
	if p.Type == "stat" {
		defaults["noValue"] = cmp.Or(p.NoValue, "No data")
	}
	if p.Neutral {
		defaults["color"] = obj{"mode": "fixed", "fixedColor": "text"}
	}
	if p.Min != nil {
		defaults["min"] = *p.Min
	}
	if p.Max != nil {
		defaults["max"] = *p.Max
	}
	if p.Decimals != nil {
		defaults["decimals"] = *p.Decimals
	}
	if len(p.Mappings) > 0 {
		res := obj{}
		for idx, m := range p.Mappings {
			res[strconv.Itoa(m.Value)] = obj{"text": m.Text, "color": m.Color, "index": idx}
		}
		defaults["mappings"] = []any{obj{"type": "value", "options": res}}
		if len(p.Thresholds) == 0 {
			defaults["thresholds"] = obj{"mode": "absolute", "steps": []thresholdStep{{Color: "text"}}}
		}
		defaults["color"] = obj{"mode": "thresholds"}
		if p.Type == "state-timeline" {
			defaults["color"] = obj{"mode": "fixed", "fixedColor": "text"}
		}
	}
	if p.Link != "" {
		defaults["links"] = []dataLink{{
			Title: "Open " + p.Link,
			URL:   r.dashboardPath(p.Link) + "?${__url_time_range}&${__all_variables}",
		}}
	}
	if p.CodeLinks {
		defaults["links"] = []dataLink{{
			Title: "Reply codes explained",
			URL:   r.dashboardPath(codesName) + "?${__url_time_range}&${__all_variables}",
		}}
	}
	if custom := p.customConfig(); custom != nil {
		defaults["custom"] = custom
	}
	return defaults
}

func (p Panel) customConfig() obj {
	if p.Type == "state-timeline" {
		return obj{"lineWidth": 0, "fillOpacity": 80}
	}
	if p.Type != "timeseries" {
		return nil
	}
	custom := obj{
		"drawStyle":         "line",
		"lineWidth":         2,
		"fillOpacity":       0,
		"showPoints":        "never",
		"spanNulls":         false,
		"axisPlacement":     "auto",
		"stacking":          obj{"mode": "none", "group": "A"},
		"thresholdsStyle":   obj{"mode": "off"},
		"scaleDistribution": obj{"type": "linear"},
	}
	if p.Stack != "" {
		custom["stacking"] = obj{"mode": p.Stack, "group": "A"}
		custom["fillOpacity"] = 60
		custom["lineWidth"] = 1
	}
	if p.LogScale {
		custom["scaleDistribution"] = obj{"type": "log", "log": 10}
	}
	if p.ThresholdUI != "" {
		custom["thresholdsStyle"] = obj{"mode": p.ThresholdUI}
	}
	return custom
}

func (p Panel) panelOptions() obj {
	options := obj{}
	switch p.Type {
	case "timeseries":
		options = obj{
			"legend":  obj{"displayMode": "list", "placement": "bottom", "showLegend": true},
			"tooltip": obj{"mode": "multi", "sort": "desc"},
		}
	case "stat":
		colorMode := "value"
		if p.Neutral {
			colorMode = "none"
		}
		options = obj{
			"reduceOptions": obj{"calcs": []any{"lastNotNull"}, "fields": "", "values": false},
			"colorMode":     colorMode,
			"graphMode":     "none",
			"justifyMode":   "auto",
			"textMode":      "auto",
			"orientation":   "auto",
		}
	case "bargauge":
		options = obj{
			"reduceOptions": obj{"calcs": []any{"lastNotNull"}, "fields": "", "values": false},
			"displayMode":   "basic",
			"orientation":   "horizontal",
			"showUnfilled":  true,
		}
	case "state-timeline":
		options = obj{
			"showValue":   "never",
			"rowHeight":   0.9,
			"mergeValues": true,
			"alignValue":  "left",
			"legend":      obj{"displayMode": "list", "placement": "bottom", "showLegend": true},
			"tooltip":     obj{"mode": "single", "sort": "none"},
		}
	case "table":
		options = obj{"showHeader": true, "cellHeight": "sm"}
	case "text":
		options = obj{"mode": "markdown", "content": ""}
	}
	for k, v := range p.Options {
		options[k] = v
	}
	return options
}
