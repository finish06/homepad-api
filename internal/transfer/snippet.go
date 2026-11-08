package transfer

import (
	"bytes"
	"net/url"
	"strings"

	"gopkg.in/yaml.v3"
)

// SPEC-backup-import §4.3 — the Gatus endpoints snippet (OQ-9 b).

type gatusEndpoint struct {
	Name       string   `yaml:"name"`
	Group      string   `yaml:"group"`
	URL        string   `yaml:"url"`
	Interval   string   `yaml:"interval"`
	Conditions []string `yaml:"conditions"`
}

// monitorFor turns an entry's siteMonitor (preferred) or ping into a Gatus
// endpoint, or nil when it has neither, or one Homepad can't use.
func monitorFor(e HomepageEntry) *gatusEndpoint {
	if sm := strings.TrimSpace(e.SiteMonitor); sm != "" && !hasHomepageVariable(sm) && IsHTTPURL(sm) {
		return &gatusEndpoint{URL: sm, Interval: "60s", Conditions: []string{"[STATUS] < 400"}}
	}
	host := strings.TrimSpace(e.Ping)
	if host == "" || hasHomepageVariable(host) {
		return nil
	}
	if strings.Contains(host, "://") {
		u, err := url.Parse(host)
		if err != nil {
			return nil
		}
		host = u.Hostname()
	}
	if host == "" || strings.ContainsAny(host, " /") {
		return nil
	}
	return &gatusEndpoint{URL: "icmp://" + host, Interval: "60s", Conditions: []string{"[CONNECTED] == true"}}
}

// gatusSnippet renders the endpoints as YAML ready to paste into Gatus's
// config ("" when there are none).
func gatusSnippet(eps []gatusEndpoint) string {
	if len(eps) == 0 {
		return ""
	}
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(map[string][]gatusEndpoint{"endpoints": eps}); err != nil {
		panic("transfer: gatus snippet: " + err.Error())
	}
	_ = enc.Close()
	return b.String()
}
