// Package docs holds the dashboard's in-app manual (PRD §25.4, ADR-072): a
// directory of Markdown files with a YAML front-matter, embedded in the binary
// and parsed once at boot into what the API serves.
//
// The corpus lives beside this parser rather than under docs/ because go:embed
// cannot reach above its own package directory. They are still ordinary
// Markdown files, reviewed like any other prose.
package docs

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
	"gopkg.in/yaml.v3"
)

//go:embed manual/*.md
var manualFS embed.FS

// Topic is one chapter: its front-matter, the prose before the first heading,
// and its `##` sections in document order.
type Topic struct {
	ID         string    `json:"id"`
	Title      string    `json:"title"`
	Icon       string    `json:"icon,omitempty"`
	Group      string    `json:"group"`
	Summary    string    `json:"summary"`
	Permission string    `json:"permission,omitempty"`
	Root       bool      `json:"root,omitempty"`
	Links      []Link    `json:"links,omitempty"`
	IntroHTML  string    `json:"intro_html,omitempty"`
	IntroText  string    `json:"intro_text,omitempty"`
	Sections   []Section `json:"sections"`

	// order places the topic inside its group. Not serialised: the API returns
	// the manual already in reading order, so no client re-sorts it and none
	// can drift from the order the author chose.
	order int
}

// Section is a `##` heading and everything under it, up to the next one.
type Section struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Permission string `json:"permission,omitempty"`
	Root       bool   `json:"root,omitempty"`
	HTML       string `json:"html"`
	Text       string `json:"text"`
}

// Link points out of the manual: a dashboard route, or an absolute URL.
type Link struct {
	Label string `json:"label"`
	Route string `json:"route,omitempty"`
	Href  string `json:"href,omitempty"`
}

// frontMatter is the YAML header of a topic file. `gates` maps a section id to
// what that section needs ON TOP of its topic — a permission string, or the
// literal `root: true`. A nil value is a no-op, so a key may be written down
// while its gate is still being decided.
type frontMatter struct {
	ID         string            `yaml:"id"`
	Title      string            `yaml:"title"`
	Icon       string            `yaml:"icon"`
	Group      string            `yaml:"group"`
	Summary    string            `yaml:"summary"`
	Order      int               `yaml:"order"`
	Permission string            `yaml:"permission"`
	Root       bool              `yaml:"root"`
	Gates      map[string]string `yaml:"gates"`
	Links      []Link            `yaml:"links"`
}

// Manual is the parsed corpus, in the order the groups are meant to be read.
type Manual struct {
	Topics []Topic
}

// Validator is what a caller supplies to have the corpus checked against the
// rest of the build: the permission catalogue, the icons that ship, the routes
// that exist. Kept as functions rather than as imports so this package does not
// pull the dashboard's asset list or the auth catalogue into every consumer —
// and so a test can validate a fixture without either.
type Validator struct {
	KnownPermission func(string) bool
	KnownIcon       func(string) bool
	KnownRoute      func(string) bool
}

// Load parses the embedded manual. It is called once at boot: a malformed
// corpus is a build that must not serve, not a page that renders badly.
func Load(v Validator) (*Manual, error) {
	return load(manualFS, "manual", v)
}

func load(fsys fs.FS, dir string, v Validator) (*Manual, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("manual: %w", err)
	}
	m := &Manual{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		name := path.Join(dir, e.Name())
		raw, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		t, err := parseTopic(e.Name(), raw, v)
		if err != nil {
			return nil, err
		}
		// No duplicate check: parseTopic requires the id to equal the file name,
		// and a directory cannot hold the same name twice. Uniqueness is the
		// file system's, which is why it is not re-asserted here.
		m.Topics = append(m.Topics, *t)
	}
	if len(m.Topics) == 0 {
		return nil, fmt.Errorf("manual: no topic found in %s", dir)
	}
	// Reading order, decided by the author and applied here: the group's rank,
	// then `order:` within it. Files are read alphabetically, which is not the
	// order anyone wants to read a manual in.
	sort.SliceStable(m.Topics, func(i, j int) bool {
		gi, _ := groupRank(m.Topics[i].Group)
		gj, _ := groupRank(m.Topics[j].Group)
		if gi != gj {
			return gi < gj
		}
		if m.Topics[i].order != m.Topics[j].order {
			return m.Topics[i].order < m.Topics[j].order
		}
		return m.Topics[i].Title < m.Topics[j].Title
	})
	return m, nil
}

// groupOrder is the reading order of the manual's task groups (§25.4). A group
// outside this list is a typo, not a new section of the manual: the parse says
// so rather than inventing a heading nobody meant to write.
var groupOrder = []string{
	"Start here",
	"Ship code",
	"Run and debug",
	"Automate",
	"Your account and team",
	"Instance administration",
}

func groupRank(group string) (int, bool) {
	for i, g := range groupOrder {
		if g == group {
			return i, true
		}
	}
	return 0, false
}

var frontMatterRE = regexp.MustCompile(`(?s)\A---\r?\n(.*?)\r?\n---\r?\n?`)

func parseTopic(file string, raw []byte, v Validator) (*Topic, error) {
	match := frontMatterRE.FindSubmatchIndex(raw)
	if match == nil {
		return nil, fmt.Errorf("%s: missing YAML front-matter — the file must open with a `---` block", file)
	}
	var fm frontMatter
	if err := yaml.Unmarshal(raw[match[2]:match[3]], &fm); err != nil {
		return nil, fmt.Errorf("%s: front-matter: %w", file, err)
	}
	body := raw[match[1]:]

	if err := validateFrontMatter(file, fm, v); err != nil {
		return nil, err
	}
	if want := strings.TrimSuffix(file, ".md"); fm.ID != want {
		return nil, fmt.Errorf("%s: id is %q — the file name is the id, so this file must be %s.md", file, fm.ID, fm.ID)
	}

	t := &Topic{
		ID: fm.ID, Title: fm.Title, Icon: fm.Icon, Group: fm.Group,
		Summary: fm.Summary, Permission: fm.Permission, Root: fm.Root, Links: fm.Links,
		order: fm.Order,
	}
	intro, sections, err := splitSections(file, body)
	if err != nil {
		return nil, err
	}
	if t.IntroHTML, t.IntroText, err = render(file, intro); err != nil {
		return nil, err
	}
	ids := map[string]bool{}
	for _, s := range sections {
		if ids[s.ID] {
			return nil, fmt.Errorf("%s: two sections slug to %q — anchors and gates key on that slug", file, s.ID)
		}
		ids[s.ID] = true
		if s.HTML, s.Text, err = render(file, s.body); err != nil {
			return nil, err
		}
		if gate, ok := fm.Gates[s.ID]; ok {
			switch {
			case gate == "" || gate == "null":
				// Written down, gated on nothing: a no-op, kept legal so a key
				// can be parked while its gate is being decided.
			case gate == "root":
				s.Root = true
			case v.KnownPermission != nil && !v.KnownPermission(gate):
				return nil, fmt.Errorf("%s: section %q is gated on %q, which is not in the permission catalogue", file, s.ID, gate)
			default:
				s.Permission = gate
			}
		}
		t.Sections = append(t.Sections, s.Section)
	}
	for key := range fm.Gates {
		if !ids[key] {
			return nil, fmt.Errorf("%s: gate on section %q, which does not exist — a gate on nothing protects nothing", file, key)
		}
	}
	return t, nil
}

func validateFrontMatter(file string, fm frontMatter, v Validator) error {
	for field, value := range map[string]string{"id": fm.ID, "title": fm.Title, "group": fm.Group, "summary": fm.Summary} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s: front-matter is missing `%s`", file, field)
		}
	}
	if _, ok := groupRank(fm.Group); !ok {
		return fmt.Errorf("%s: group %q is not one of the manual's groups (%s)", file, fm.Group, strings.Join(groupOrder, ", "))
	}
	if v.KnownIcon != nil && fm.Icon != "" && !v.KnownIcon(fm.Icon) {
		return fmt.Errorf("%s: icon %q does not ship with the dashboard", file, fm.Icon)
	}
	if v.KnownPermission != nil && fm.Permission != "" && !v.KnownPermission(fm.Permission) {
		return fmt.Errorf("%s: permission %q is not in the catalogue", file, fm.Permission)
	}
	for _, l := range fm.Links {
		switch {
		case strings.TrimSpace(l.Label) == "":
			return fmt.Errorf("%s: a link has no label", file)
		case l.Route != "" && l.Href != "":
			return fmt.Errorf("%s: link %q has both a route and an href", file, l.Label)
		case l.Route == "" && l.Href == "":
			return fmt.Errorf("%s: link %q points nowhere", file, l.Label)
		case l.Href != "" && !strings.HasPrefix(l.Href, "https://"):
			return fmt.Errorf("%s: link %q must be an absolute https URL, got %q", file, l.Label, l.Href)
		case l.Route != "" && v.KnownRoute != nil && !v.KnownRoute(l.Route):
			return fmt.Errorf("%s: link %q points at %q, which is not a declared dashboard route", file, l.Label, l.Route)
		}
	}
	return nil
}

// sectionSource is a section and the Markdown it was cut from.
type sectionSource struct {
	Section
	body []byte
}

var headingRE = regexp.MustCompile(`(?m)^## +(.+?)[ \t]*$`)

// splitSections cuts the body at its `##` headings. Done on the source rather
// than on the parsed AST because each section is rendered on its own: the API
// serves a section at a time so the client can gate, anchor and search them
// individually.
func splitSections(file string, body []byte) ([]byte, []sectionSource, error) {
	matches := headingRE.FindAllSubmatchIndex(body, -1)
	if len(matches) == 0 {
		return body, nil, nil
	}
	intro := body[:matches[0][0]]
	var out []sectionSource
	for i, m := range matches {
		end := len(body)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		title := strings.TrimSpace(string(body[m[2]:m[3]]))
		id := slug(title)
		if id == "" {
			return nil, nil, fmt.Errorf("%s: heading %q slugs to nothing", file, title)
		}
		out = append(out, sectionSource{
			Section: Section{ID: id, Title: title},
			body:    body[m[1]:end],
		})
	}
	return intro, out, nil
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// slug is the section id, the anchor, and the key `gates` uses. Inline markup
// is stripped first so that `## The **build** pack` and `## The build pack`
// cannot slug differently.
func slug(title string) string {
	s := strings.ToLower(title)
	s = strings.NewReplacer("`", "", "*", "", "_", "").Replace(s)
	s = nonSlug.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

// md is the renderer. Raw HTML is NOT enabled: goldmark DROPS any markup
// written in a source file instead of passing it through, which is what makes
// the output safe to bind (ADR-072 §3). Never add html.WithUnsafe() here — the
// safety of the whole feature is this one absent option.
var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithParserOptions(parser.WithASTTransformers(
		util.Prioritized(calloutClass{}, 100),
	)),
)

func render(file string, src []byte) (html, plain string, err error) {
	src = bytes.TrimSpace(src)
	if len(src) == 0 {
		return "", "", nil
	}
	var buf bytes.Buffer
	if err := md.Convert(src, &buf); err != nil {
		return "", "", fmt.Errorf("%s: %w", file, err)
	}
	return strings.TrimSpace(buf.String()), plainText(src), nil
}

// plainText is what the client searches: the prose without its markup. Taken
// from the AST's text segments rather than by stripping tags out of the HTML,
// so a word never comes back glued to an attribute.
func plainText(src []byte) string {
	doc := md.Parser().Parse(text.NewReader(src))
	var b strings.Builder
	_ = walkText(doc, src, &b)
	return strings.Join(strings.Fields(b.String()), " ")
}

// walkText collects the AST's text segments. Code blocks carry their lines
// rather than a Text node, so they are collected explicitly — a developer
// searching for a flag they saw in a snippet should find the page.
func walkText(n ast.Node, src []byte, b *strings.Builder) error {
	return ast.Walk(n, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := node.(type) {
		case *ast.Text:
			seg := n.Segment
			b.Write(seg.Value(src))
			b.WriteByte(' ')
		case *ast.FencedCodeBlock, *ast.CodeBlock:
			lines := node.Lines()
			for i := 0; i < lines.Len(); i++ {
				seg := lines.At(i)
				b.Write(seg.Value(src))
				b.WriteByte(' ')
			}
		}
		return ast.WalkContinue, nil
	})
}

// calloutClass marks a blockquote that opens with **Note** or **Warning** so a
// warning does not render as a remark. The old block vocabulary had two kinds
// with two colours; Markdown has one blockquote, and losing the distinction
// would flatten "this destroys data" into "by the way".
//
// It runs on the AST rather than on the HTML: the class lands on the node, and
// the renderer writes it, so nothing here parses its own output.
type calloutClass struct{}

func (calloutClass) Transform(doc *ast.Document, reader text.Reader, _ parser.Context) {
	src := reader.Source()
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		quote, ok := n.(*ast.Blockquote)
		if !ok {
			return ast.WalkContinue, nil
		}
		var lead strings.Builder
		_ = walkText(quote, src, &lead)
		switch first := strings.TrimSpace(lead.String()); {
		case strings.HasPrefix(first, "Warning"):
			quote.SetAttributeString("class", []byte("akd-warn"))
		case strings.HasPrefix(first, "Note"):
			quote.SetAttributeString("class", []byte("akd-note"))
		}
		return ast.WalkSkipChildren, nil
	})
}
