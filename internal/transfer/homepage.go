package transfer

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// SPEC-backup-import §4 — reading Homepage's services.yaml, bookmarks.yaml
// and settings.yaml (gethomepage/homepage docs/configs/*.md and
// src/utils/config/service-helpers.js, 2026-11-07).

const (
	// MaxHomepageFileBytes caps each pasted or uploaded file (AC-026).
	MaxHomepageFileBytes = 1 << 20
	// MaxGroupDepth is the deepest nesting of groups accepted (§4.5).
	MaxGroupDepth = 8
)

// HomepageInput is the request body of the Homepage import routes.
type HomepageInput struct {
	Services  string `json:"services"`
	Bookmarks string `json:"bookmarks,omitempty"`
	Settings  string `json:"settings,omitempty"`
}

// Homepage is what the parser keeps from the three files. Widget blocks,
// `providers` and every other value the import doesn't use are dropped while
// parsing and never reach this struct (§9, AC-022).
type Homepage struct {
	// Sections is every group, in file order: services.yaml first, a parent
	// before its nested groups, then bookmarks.yaml groups not seen yet.
	Sections []HomepageSection
	// Entries is every service and bookmark in Homepage order: per section,
	// services by weight then file order, then the section's bookmarks.
	Entries []HomepageEntry
	// Layout is settings.yaml `layout`'s top-level group names, in order.
	Layout []string
	// Target is settings.yaml `target` ("" when unset).
	Target string
	// IgnoredSettings is every other settings.yaml key, by name only.
	IgnoredSettings []string
}

// HomepageSection is one group. Name is the full "Parent / Child" path.
type HomepageSection struct {
	Name string
	Root string
}

// HomepageEntry is one service or bookmark. Only scalar values the mapping
// uses are kept.
type HomepageEntry struct {
	Section     string
	Name        string
	Href        string
	Description string
	Icon        string
	Target      string
	SiteMonitor string
	Ping        string
	Bookmark    bool
	// NoFields is true when the entry had no field map (Homepage skips it).
	NoFields bool
	// Widget is true when the entry had `widget` or `widgets`. Their values
	// are never read.
	Widget     bool
	Docker     bool // server, container or showStats
	Kubernetes bool // app, namespace or podSelector
	// IgnoredKeys counts keys the import doesn't use (statusStyle, id, …).
	IgnoredKeys int
	Line        int
}

// maxNodeVisits caps how many YAML nodes the parser will visit, counting each
// alias expansion again. A real 1 MB file is far under it; an alias bomb
// ("billion laughs") hits it at once instead of being expanded (§4.5).
const maxNodeVisits = 1_000_000

const (
	fileServices  = "services"
	fileBookmarks = "bookmarks"
	fileSettings  = "settings"
)

var (
	shapeCode = map[string]string{
		fileServices:  CodeNotHomepageServices,
		fileBookmarks: CodeNotHomepageBookmark,
		fileSettings:  CodeNotHomepageSettings,
	}
	shapeMessage = map[string]string{
		fileServices:  "This doesn't look like a Homepage services.yaml. It should be a list of groups.",
		fileBookmarks: "This doesn't look like a Homepage bookmarks.yaml. It should be a list of groups.",
		fileSettings:  "This doesn't look like a Homepage settings.yaml. It should be a map of settings.",
	}
	yamlLineRe = regexp.MustCompile(`line (\d+):\s*(.*)`)
	// standardTags are the YAML core tags. Anything else is a custom tag.
	standardTags = map[string]bool{
		"": true, "!!str": true, "!!int": true, "!!float": true, "!!bool": true, "!!null": true,
		"!!map": true, "!!seq": true, "!!timestamp": true, "!!binary": true, "!!merge": true,
	}
)

func shapeErr(file string, line int) *Error {
	return &Error{Status: http.StatusUnprocessableEntity, Code: shapeCode[file], Message: shapeMessage[file], File: file, Line: line}
}

func syntaxErr(file string, line int, detail string) *Error {
	detail = strings.TrimSuffix(strings.TrimSpace(detail), ".")
	msg := "There's a problem in this file: " + detail + "."
	if line > 0 {
		msg = fmt.Sprintf("There's a problem on line %d: %s.", line, detail)
	}
	return &Error{Status: http.StatusBadRequest, Code: CodeYAMLSyntax, Message: msg, File: file, Line: line, Detail: detail}
}

// ParseHomepage parses the three files. services is required; bookmarks and
// settings may be empty. Every file is size-checked, then syntax- and
// tag-checked, before any of them is read for its shape.
func ParseHomepage(in HomepageInput) (*Homepage, *Error) {
	files := []struct{ name, text string }{
		{fileServices, in.Services}, {fileBookmarks, in.Bookmarks}, {fileSettings, in.Settings},
	}
	roots := map[string]*yaml.Node{}
	for _, f := range files {
		if len(f.text) > MaxHomepageFileBytes {
			return nil, &Error{Status: http.StatusRequestEntityTooLarge, Code: CodeTooLarge,
				Message: "This file is too big. Homepage files can be up to 1 MB.", File: f.name}
		}
	}
	for _, f := range files {
		root, err := loadYAML(f.name, f.text)
		if err != nil {
			return nil, err
		}
		roots[f.name] = root
	}
	if roots[fileServices] == nil {
		return nil, shapeErr(fileServices, 1)
	}
	p := &hpParser{hp: &Homepage{}, byName: map[string]*hpSection{}}
	if err := p.services(roots[fileServices]); err != nil {
		return nil, err
	}
	if root := roots[fileBookmarks]; root != nil {
		if err := p.bookmarks(root); err != nil {
			return nil, err
		}
	}
	if root := roots[fileSettings]; root != nil {
		if err := p.settings(root); err != nil {
			return nil, err
		}
	}
	for _, sec := range p.order {
		p.hp.Sections = append(p.hp.Sections, HomepageSection{Name: sec.name, Root: sec.root})
		sort.SliceStable(sec.services, func(i, j int) bool { return sec.services[i].weight < sec.services[j].weight })
		for _, e := range sec.services {
			p.hp.Entries = append(p.hp.Entries, e.HomepageEntry)
		}
		p.hp.Entries = append(p.hp.Entries, sec.bookmarks...)
	}
	return p.hp, nil
}

// loadYAML decodes one file into a node tree (nil for an empty file) and
// walks all of it once: custom tags are refused, and alias expansion is
// capped by maxNodeVisits.
func loadYAML(file, text string) (*yaml.Node, *Error) {
	var doc yaml.Node
	err := yaml.NewDecoder(strings.NewReader(text)).Decode(&doc)
	if errors.Is(err, io.EOF) {
		return nil, nil
	}
	if err != nil {
		hint, detail := yamlErrorParts(err)
		return nil, syntaxErr(file, refineLine(text, hint, detail), detail)
	}
	visits := 0
	var walk func(n *yaml.Node) *Error
	walk = func(n *yaml.Node) *Error {
		visits++
		if visits > maxNodeVisits {
			return syntaxErr(file, n.Line, "it uses too many aliases")
		}
		if !standardTags[n.Tag] {
			return syntaxErr(file, n.Line, "custom tags like "+safeTag(n.Tag)+" aren't supported")
		}
		if n.Kind == yaml.AliasNode {
			if n.Alias == nil {
				return syntaxErr(file, n.Line, "an alias points at nothing")
			}
			return walk(n.Alias)
		}
		for _, c := range n.Content {
			if err := walk(c); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(&doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return nil, nil
	}
	root := resolve(doc.Content[0])
	if root.Kind == yaml.ScalarNode && root.Tag == "!!null" {
		return nil, nil
	}
	return root, nil
}

// yamlErrorParts splits a yaml.v3 error into its line (0 when it names
// none) and its detail.
func yamlErrorParts(err error) (int, string) {
	msg := strings.TrimPrefix(err.Error(), "yaml: ")
	if m := yamlLineRe.FindStringSubmatch(msg); m != nil {
		line, _ := strconv.Atoi(m[1])
		return line, m[2]
	}
	return 0, msg
}

// refineLine turns yaml.v3's line into the line the problem is on. yaml.v3
// reports a parser error at the line before (its context mark, 0-based) and
// gives no line for line 1. So: the first of hint and hint+1 whose prefix of
// the file fails with the same detail is the line. Two prefix parses at most,
// so a 1 MB file stays cheap.
func refineLine(text string, hint int, detail string) int {
	lines := strings.SplitAfter(text, "\n")
	for l := max(hint, 1); l <= hint+1 && l <= len(lines); l++ {
		var n yaml.Node
		err := yaml.NewDecoder(strings.NewReader(strings.Join(lines[:l], ""))).Decode(&n)
		if err == nil {
			continue
		}
		if _, d := yamlErrorParts(err); d == detail {
			return l
		}
	}
	return max(hint, 1)
}

// safeTag keeps a tag short and printable for the message.
func safeTag(tag string) string {
	if len(tag) > 24 {
		tag = tag[:24]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, tag)
}

// resolve follows an alias to the node it names.
func resolve(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n
}

func isNull(n *yaml.Node) bool {
	return n == nil || (n.Kind == yaml.ScalarNode && n.Tag == "!!null")
}

type kv struct {
	key  string
	line int
	val  *yaml.Node
}

// pairs lists a mapping's keys in order, with YAML merge keys ("<<: *x")
// expanded. An explicit key beats a merged one.
func pairs(n *yaml.Node) []kv {
	n = resolve(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	var out, merged []kv
	seen := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := resolve(n.Content[i]), n.Content[i+1]
		if k.Tag == "!!merge" {
			src := resolve(v)
			srcs := []*yaml.Node{src}
			if src != nil && src.Kind == yaml.SequenceNode {
				srcs = src.Content
			}
			for _, m := range srcs {
				merged = append(merged, pairs(m)...)
			}
			continue
		}
		if seen[k.Value] {
			continue
		}
		seen[k.Value] = true
		out = append(out, kv{key: k.Value, line: k.Line, val: v})
	}
	for _, m := range merged {
		if !seen[m.key] {
			seen[m.key] = true
			out = append(out, m)
		}
	}
	return out
}

// scalar is a node's text when it is a scalar, else "".
func scalar(n *yaml.Node) string {
	n = resolve(n)
	if n == nil || n.Kind != yaml.ScalarNode || n.Tag == "!!null" {
		return ""
	}
	return n.Value
}

type hpSection struct {
	name, root string
	services   []weighted
	bookmarks  []HomepageEntry
	counter    int // services so far, for Homepage's default weight
}

type weighted struct {
	HomepageEntry
	weight int
}

type hpParser struct {
	hp      *Homepage
	order   []*hpSection
	byName  map[string]*hpSection
	entries int
}

func (p *hpParser) section(name, root string) (*hpSection, *Error) {
	if s, ok := p.byName[name]; ok {
		return s, nil
	}
	if len(p.order) >= MaxSections {
		return nil, tooMany("sections", len(p.order)+1, MaxSections)
	}
	s := &hpSection{name: name, root: root}
	p.byName[name] = s
	p.order = append(p.order, s)
	return s, nil
}

func (p *hpParser) countEntry() *Error {
	p.entries++
	if p.entries > MaxApps {
		return tooMany("apps", p.entries, MaxApps)
	}
	return nil
}

// groupList reads a file's top level: a list of one-key maps, group name →
// entries.
func groupList(file string, root *yaml.Node) ([]kv, *Error) {
	if root.Kind != yaml.SequenceNode {
		return nil, shapeErr(file, root.Line)
	}
	var out []kv
	for _, item := range root.Content {
		g := resolve(item)
		ps := pairs(g)
		if len(ps) == 0 {
			return nil, shapeErr(file, g.Line)
		}
		out = append(out, ps[0])
	}
	return out, nil
}

func (p *hpParser) services(root *yaml.Node) *Error {
	groups, err := groupList(fileServices, root)
	if err != nil {
		return err
	}
	for _, g := range groups {
		name := strings.TrimSpace(g.key)
		if err := p.serviceGroup(name, name, g.val, g.line, 1); err != nil {
			return err
		}
	}
	return nil
}

func (p *hpParser) serviceGroup(path, root string, val *yaml.Node, line, depth int) *Error {
	if depth > MaxGroupDepth {
		return &Error{Status: http.StatusUnprocessableEntity, Code: CodeTooDeep,
			Message: fmt.Sprintf("Groups are nested more than %d deep.", MaxGroupDepth), File: fileServices, Line: line}
	}
	sec, err := p.section(path, root)
	if err != nil {
		return err
	}
	v := resolve(val)
	if isNull(v) {
		return nil
	}
	if v.Kind != yaml.SequenceNode {
		return shapeErr(fileServices, v.Line)
	}
	for _, item := range v.Content {
		e := resolve(item)
		ps := pairs(e)
		if len(ps) == 0 {
			return shapeErr(fileServices, e.Line)
		}
		name, body := strings.TrimSpace(ps[0].key), resolve(ps[0].val)
		if body != nil && body.Kind == yaml.SequenceNode {
			if err := p.serviceGroup(path+" / "+name, root, body, ps[0].line, depth+1); err != nil {
				return err
			}
			continue
		}
		if err := p.countEntry(); err != nil {
			return err
		}
		entry := HomepageEntry{Section: path, Name: name, Line: ps[0].line}
		// Homepage's default weight is the entry's position among the
		// group's services × 100; an entry it can't read doesn't count.
		weight := (sec.counter + 1) * 100
		if body == nil || body.Kind != yaml.MappingNode {
			entry.NoFields = true
		} else {
			sec.counter++
			for _, f := range pairs(body) {
				switch f.key {
				case "href":
					entry.Href = scalar(f.val)
				case "description":
					entry.Description = scalar(f.val)
				case "icon":
					entry.Icon = scalar(f.val)
				case "target":
					entry.Target = scalar(f.val)
				case "siteMonitor":
					entry.SiteMonitor = scalar(f.val)
				case "ping":
					entry.Ping = scalar(f.val)
				case "weight":
					weight = 0
					if w, err := strconv.Atoi(strings.TrimSpace(scalar(f.val))); err == nil {
						weight = w
					}
				case "widget", "widgets":
					entry.Widget = true // the value (often an API key) is never read
				case "server", "container", "showStats":
					entry.Docker = true
				case "app", "namespace", "podSelector":
					entry.Kubernetes = true
				default:
					entry.IgnoredKeys++
				}
			}
		}
		sec.services = append(sec.services, weighted{HomepageEntry: entry, weight: weight})
	}
	return nil
}

func (p *hpParser) bookmarks(root *yaml.Node) *Error {
	groups, err := groupList(fileBookmarks, root)
	if err != nil {
		return err
	}
	for _, g := range groups {
		name := strings.TrimSpace(g.key)
		sec, err := p.section(name, name)
		if err != nil {
			return err
		}
		v := resolve(g.val)
		if isNull(v) {
			continue
		}
		if v.Kind != yaml.SequenceNode {
			return shapeErr(fileBookmarks, v.Line)
		}
		for _, item := range v.Content {
			e := resolve(item)
			ps := pairs(e)
			if len(ps) == 0 {
				return shapeErr(fileBookmarks, e.Line)
			}
			if err := p.countEntry(); err != nil {
				return err
			}
			entry := HomepageEntry{Section: name, Name: strings.TrimSpace(ps[0].key), Line: ps[0].line, Bookmark: true}
			body := resolve(ps[0].val)
			if body != nil && body.Kind == yaml.SequenceNode && len(body.Content) > 0 {
				body = resolve(body.Content[0])
			}
			if body == nil || body.Kind != yaml.MappingNode {
				entry.NoFields = true
			} else {
				for _, f := range pairs(body) {
					switch f.key {
					case "href":
						entry.Href = scalar(f.val)
					case "description":
						entry.Description = scalar(f.val)
					case "icon":
						entry.Icon = scalar(f.val)
					case "abbr":
						// The initials badge does the same job.
					default:
						entry.IgnoredKeys++
					}
				}
			}
			sec.bookmarks = append(sec.bookmarks, entry)
		}
	}
	return nil
}

func (p *hpParser) settings(root *yaml.Node) *Error {
	if root.Kind != yaml.MappingNode {
		return shapeErr(fileSettings, root.Line)
	}
	for _, f := range pairs(root) {
		switch f.key {
		case "layout":
			v := resolve(f.val)
			switch {
			case v == nil:
			case v.Kind == yaml.MappingNode:
				for _, g := range pairs(v) {
					p.hp.Layout = append(p.hp.Layout, strings.TrimSpace(g.key))
				}
			case v.Kind == yaml.SequenceNode:
				for _, item := range v.Content {
					if ps := pairs(item); len(ps) > 0 {
						p.hp.Layout = append(p.hp.Layout, strings.TrimSpace(ps[0].key))
					} else if s := scalar(item); s != "" {
						p.hp.Layout = append(p.hp.Layout, strings.TrimSpace(s))
					}
				}
			}
		case "target":
			p.hp.Target = strings.TrimSpace(scalar(f.val))
		default:
			// By name only: a value (providers holds API keys) is never read.
			p.hp.IgnoredSettings = append(p.hp.IgnoredSettings, f.key)
		}
	}
	return nil
}
