package app

import (
	"bufio"
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type ruleFile struct {
	Groups []struct {
		Name  string `yaml:"name"`
		Rules []struct {
			Alert       string            `yaml:"alert"`
			Expr        string            `yaml:"expr"`
			For         string            `yaml:"for"`
			Labels      map[string]string `yaml:"labels"`
			Annotations map[string]string `yaml:"annotations"`
		} `yaml:"rules"`
	} `yaml:"groups"`
}

// promQL words that are not metric names.
var promWords = map[string]bool{
	"sum": true, "rate": true, "increase": true, "histogram_quantile": true, "by": true, "without": true,
	"and": true, "or": true, "unless": true, "on": true, "ignoring": true, "bool": true, "max": true,
	"min": true, "avg": true, "count": true, "clamp_min": true, "group_left": true, "group_right": true, "offset": true,
}

// GitHub-style heading anchor.
func slug(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	var b strings.Builder
	for _, r := range h {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
	}
	return b.String()
}

// TestAlertRulesReferenceRealMetrics parses deploy/prometheus/alerts.yml and
// checks every metric (and label key) in each expression against what a
// running app actually exports on /metrics, and every runbook link against
// docs/RUNBOOK.md headings.
func TestAlertRulesReferenceRealMetrics(t *testing.T) {
	raw, err := os.ReadFile("../deploy/prometheus/alerts.yml")
	if err != nil {
		t.Fatal(err)
	}
	var rf ruleFile
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&rf); err != nil {
		t.Fatalf("alerts.yml: %v", err)
	}

	c := testConfig(t)
	c.Auth.Web.Enabled = true
	a := newTestApp(t, c)
	srv := httptest.NewServer(a.Handler(Hooks{}))
	defer srv.Close()
	for _, p := range []string{"/healthz", "/nope"} { // create http_* series
		res, _ := http.Get(srv.URL + p)
		res.Body.Close()
	}
	var exposition bytes.Buffer
	a.Metrics.Render(&exposition)
	series := map[string][]string{} // metric name -> label strings
	sc := bufio.NewScanner(&exposition)
	for sc.Scan() {
		line := sc.Text()
		name, rest, _ := strings.Cut(strings.Fields(line)[0], "{")
		series[name] = append(series[name], rest)
	}

	rb, err := os.ReadFile("../docs/RUNBOOK.md")
	if err != nil {
		t.Fatal(err)
	}
	anchors := map[string]bool{}
	for _, l := range strings.Split(string(rb), "\n") {
		if strings.HasPrefix(l, "#") {
			anchors[slug(strings.TrimLeft(l, "#"))] = true
		}
	}

	selector := regexp.MustCompile(`([a-zA-Z_:][a-zA-Z0-9_:]*)\{([^}]*)\}`)
	labelKey := regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)\s*(=~|!=|!~|=)`)
	strip := []*regexp.Regexp{
		regexp.MustCompile(`\{[^}]*\}`),                    // label matchers
		regexp.MustCompile(`\[[^\]]*\]`),                   // range selectors
		regexp.MustCompile(`\b(by|without)\s*\([^)]*\)`),   // grouping labels
		regexp.MustCompile(`\b\d+(\.\d+)?([eE][+-]?\d+)?`), // numbers
	}
	ident := regexp.MustCompile(`[a-zA-Z_:][a-zA-Z0-9_:]*`)
	seen := map[string]bool{}
	n := 0
	for _, g := range rf.Groups {
		for _, r := range g.Rules {
			n++
			if r.Alert == "" || seen[r.Alert] {
				t.Errorf("rule without unique alert name in group %s", g.Name)
			}
			seen[r.Alert] = true
			switch r.Labels["severity"] {
			case "critical", "warning", "info":
			default:
				t.Errorf("%s: severity label missing", r.Alert)
			}
			if r.Annotations["summary"] == "" {
				t.Errorf("%s: summary missing", r.Alert)
			}
			link := r.Annotations["runbook"]
			if _, frag, ok := strings.Cut(link, "#"); !ok || !strings.HasPrefix(link, "docs/RUNBOOK.md#") || !anchors[frag] {
				t.Errorf("%s: runbook %q does not match a RUNBOOK.md heading", r.Alert, link)
			}
			for _, m := range selector.FindAllStringSubmatch(r.Expr, -1) {
				if m[1] == "up" {
					continue // synthesized by Prometheus
				}
				for _, k := range labelKey.FindAllStringSubmatch(m[2], -1) {
					found := false
					for _, ls := range series[m[1]] {
						if strings.Contains(ls, k[1]+"=") {
							found = true
						}
					}
					if !found {
						t.Errorf("%s: label %s not exported on %s", r.Alert, k[1], m[1])
					}
				}
			}
			e := r.Expr
			for _, re := range strip {
				e = re.ReplaceAllString(e, " ")
			}
			for _, id := range ident.FindAllString(e, -1) {
				if promWords[id] || id == "up" {
					continue
				}
				if _, ok := series[id]; !ok {
					t.Errorf("%s: metric %q is not exported by the app", r.Alert, id)
				}
			}
		}
	}
	if n < 10 {
		t.Fatalf("only %d rules parsed", n)
	}
}
