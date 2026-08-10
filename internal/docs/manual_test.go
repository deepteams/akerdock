package docs

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/deepteams/akerdock/internal/auth"
)

// knownPermission is the gate the boot itself applies: the catalogue is Go, so
// a permission that does not exist is caught before the binary serves.
func knownPermission(p string) bool { _, ok := auth.Catalog[p]; return ok }

// The manual that ships must parse, or the binary must not boot (ADR-072 §2).
// This test is what turns that boot failure into a CI failure instead.
func TestEmbeddedManualParses(t *testing.T) {
	m, err := Load(Validator{
		KnownPermission: knownPermission,
		KnownIcon:       dashboardIcons(t),
		KnownRoute:      dashboardRoutes(t),
	})
	if err != nil {
		t.Fatalf("the embedded manual does not parse: %v", err)
	}
	if len(m.Topics) < 20 {
		t.Fatalf("only %d topics — the corpus looks truncated", len(m.Topics))
	}
	for _, topic := range m.Topics {
		if topic.IntroHTML == "" && len(topic.Sections) == 0 {
			t.Errorf("%s: empty chapter", topic.ID)
		}
		for _, s := range topic.Sections {
			if strings.TrimSpace(s.HTML) == "" {
				t.Errorf("%s#%s: empty section", topic.ID, s.ID)
			}
			if strings.TrimSpace(s.Text) == "" {
				t.Errorf("%s#%s: no searchable text", topic.ID, s.ID)
			}
		}
	}
}

// A plain member must still find the daily surface — the invariant §25.4 states
// and the reason the manual exists at all.
func TestManualCoversTheDailySurfaceForAMember(t *testing.T) {
	m, err := Load(Validator{KnownPermission: knownPermission})
	if err != nil {
		t.Fatal(err)
	}
	member := map[string]bool{}
	for _, p := range auth.EffectivePermissions([]string{"read", "write"}) {
		member[p] = true
	}
	visible := map[string]bool{}
	for _, topic := range m.Topics {
		if topic.Root || (topic.Permission != "" && !member[topic.Permission]) {
			continue
		}
		visible[topic.ID] = true
	}
	for _, id := range []string{"applications", "env-vars", "deployments", "logs", "cli", "repository"} {
		if !visible[id] {
			t.Errorf("a member cannot see the %q chapter", id)
		}
	}
}

// §3's guarantee, asserted on the renderer rather than assumed from a flag:
// markup written in a source file comes back as text, not as markup.
func TestRawHTMLIsEscapedNotEmitted(t *testing.T) {
	fsys := fstest.MapFS{
		"m/x.md": &fstest.MapFile{Data: []byte(`---
id: x
title: X
group: Start here
summary: S
---

## Danger

Inline <script>alert(1)</script> and a <b>bold</b> tag.

<div onclick="steal()">block</div>
`)},
	}
	m, err := load(fsys, "m", Validator{})
	if err != nil {
		t.Fatal(err)
	}
	html := m.Topics[0].Sections[0].HTML
	for _, forbidden := range []string{"<script", "<b>", "<div", "onclick"} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("raw HTML reached the output (%q): %s", forbidden, html)
		}
	}
	// goldmark drops the markup entirely rather than escaping it, and keeps the
	// text around it: the sentence still reads, the tags are simply not there.
	if !strings.Contains(html, "alert(1)") || !strings.Contains(html, "bold") {
		t.Fatalf("the prose around the markup should survive, got: %s", html)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]struct{ file, body, wants string }{
		"no front-matter": {"x.md", "# just prose\n", "missing YAML front-matter"},
		"broken yaml":     {"x.md", "---\nid: [x\n---\n", "front-matter"},
		"missing title":   {"x.md", "---\nid: x\ngroup: Start here\nsummary: S\n---\n", "missing `title`"},
		"id must match the file name": {
			"other.md", "---\nid: x\ntitle: T\ngroup: Start here\nsummary: S\n---\n", "must be x.md",
		},
		"gate on a section that does not exist": {
			"x.md", "---\nid: x\ntitle: T\ngroup: Start here\nsummary: S\ngates:\n  ghost: applications:read\n---\n\n## Real\n\nText.\n",
			"does not exist",
		},
		"gate on an unknown permission": {
			"x.md", "---\nid: x\ntitle: T\ngroup: Start here\nsummary: S\ngates:\n  real: nope:read\n---\n\n## Real\n\nText.\n",
			"not in the permission catalogue",
		},
		"unknown topic permission": {
			"x.md", "---\nid: x\ntitle: T\ngroup: Start here\nsummary: S\npermission: nope:read\n---\n",
			"not in the catalogue",
		},
		"two sections with the same slug": {
			"x.md", "---\nid: x\ntitle: T\ngroup: Start here\nsummary: S\n---\n\n## The build\n\na\n\n## The **build**\n\nb\n",
			"slug to",
		},
		"link to nowhere": {
			"x.md", "---\nid: x\ntitle: T\ngroup: Start here\nsummary: S\nlinks:\n  - label: L\n---\n", "points nowhere",
		},
		"unknown group": {
			"x.md", "---\nid: x\ntitle: T\ngroup: Nowhere\nsummary: S\n---\n", "not one of the manual's groups",
		},
		"insecure link": {
			"x.md", "---\nid: x\ntitle: T\ngroup: Start here\nsummary: S\nlinks:\n  - label: L\n    href: http://example.com\n---\n",
			"absolute https URL",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fsys := fstest.MapFS{"m/" + tc.file: &fstest.MapFile{Data: []byte(tc.body)}}
			_, err := load(fsys, "m", Validator{KnownPermission: knownPermission})
			if err == nil || !strings.Contains(err.Error(), tc.wants) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.wants)
			}
			if err != nil && !strings.Contains(err.Error(), tc.file) {
				t.Errorf("the error must name the file: %v", err)
			}
		})
	}
}

func TestSectionsAndGates(t *testing.T) {
	fsys := fstest.MapFS{"m/x.md": &fstest.MapFile{Data: []byte(`---
id: x
title: T
group: Start here
summary: S
permission: applications:read
gates:
  secrets: applications:update
  parked:
  admin-only: root
---

Intro prose.

## Secrets

Gated on more than the topic.

## Parked

A key with no gate is a no-op.

## Admin only

Root-only.
`)}}
	m, err := load(fsys, "m", Validator{KnownPermission: knownPermission})
	if err != nil {
		t.Fatal(err)
	}
	topic := m.Topics[0]
	if topic.IntroText != "Intro prose." {
		t.Errorf("intro text = %q", topic.IntroText)
	}
	want := map[string]struct {
		perm string
		root bool
	}{
		"secrets":    {perm: "applications:update"},
		"parked":     {},
		"admin-only": {root: true},
	}
	if len(topic.Sections) != len(want) {
		t.Fatalf("sections = %d", len(topic.Sections))
	}
	for _, s := range topic.Sections {
		w, ok := want[s.ID]
		if !ok {
			t.Fatalf("unexpected section %q", s.ID)
		}
		if s.Permission != w.perm || s.Root != w.root {
			t.Errorf("%s: permission=%q root=%v, want %q/%v", s.ID, s.Permission, s.Root, w.perm, w.root)
		}
	}
}

// The searchable text is the prose without its markup — including what is
// inside a fence, because a developer who saw a flag in a snippet will search
// for that flag.
func TestPlainTextIsSearchable(t *testing.T) {
	fsys := fstest.MapFS{"m/x.md": &fstest.MapFile{Data: []byte(`---
id: x
title: T
group: Start here
summary: S
---

## Ports

Forward a **port** with ` + "`akerdock app port-forward`" + `:

` + "```" + `
akerdock app port-forward 15432:5432 varuna
` + "```" + `
`)}}
	m, err := load(fsys, "m", Validator{})
	if err != nil {
		t.Fatal(err)
	}
	text := m.Topics[0].Sections[0].Text
	for _, word := range []string{"Forward", "port", "akerdock app port-forward", "15432:5432"} {
		if !strings.Contains(text, word) {
			t.Errorf("search text misses %q: %q", word, text)
		}
	}
	if strings.Contains(text, "**") || strings.Contains(text, "<p>") {
		t.Errorf("search text carries markup: %q", text)
	}
}

// dashboardIcons and dashboardRoutes read the dashboard's own sources: the
// corpus is Go, the assets are TypeScript, and a manual naming an icon that
// does not ship or a route that does not exist is broken in a way only a
// cross-check can see (§25.4).
func dashboardIcons(t *testing.T) func(string) bool {
	t.Helper()
	src := readRepoFile(t, "web/src/ui/icon/icons.ts")
	re := regexp.MustCompile(`(?m)^\s+'?([a-z0-9-]+)'?:\s+[A-Z]`)
	icons := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(src, -1) {
		icons[m[1]] = true
	}
	if len(icons) < 20 {
		t.Fatalf("only %d icons parsed from icons.ts — the regex no longer matches the file", len(icons))
	}
	return func(name string) bool { return icons[name] }
}

func dashboardRoutes(t *testing.T) func(string) bool {
	t.Helper()
	src := readRepoFile(t, "web/src/app/app.routes.ts")
	re := regexp.MustCompile(`path:\s*'([^']*)'`)
	routes := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(src, -1) {
		routes["/"+strings.TrimPrefix(m[1], "/")] = true
	}
	if len(routes) < 10 {
		t.Fatalf("only %d routes parsed from app.routes.ts — the regex no longer matches the file", len(routes))
	}
	return func(route string) bool {
		// A link may point at a child route (`/applications/new`) or at a
		// parent that owns children (`/applications`); both are declared, but
		// the file spells children relative to their parent.
		if routes[route] {
			return true
		}
		for declared := range routes {
			if strings.HasSuffix(route, declared) && declared != "/" {
				return true
			}
		}
		return false
	}
}

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	// internal/docs → repository root.
	data, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("cannot read %s: %v", rel, err)
	}
	return string(data)
}

// The manual has a reading order, and it is the author's: group by group, then
// `order:` inside the group. Alphabetical filenames are not it.
func TestReadingOrder(t *testing.T) {
	m, err := Load(Validator{KnownPermission: knownPermission})
	if err != nil {
		t.Fatal(err)
	}
	var groups []string
	seen := map[string]bool{}
	lastOrder, lastGroup := -1, ""
	for _, topic := range m.Topics {
		if topic.Group != lastGroup {
			if seen[topic.Group] {
				t.Fatalf("group %q is split in two: topics of one group must be contiguous", topic.Group)
			}
			seen[topic.Group] = true
			groups = append(groups, topic.Group)
			lastGroup, lastOrder = topic.Group, -1
		}
		if topic.order < lastOrder {
			t.Errorf("%s: order %d comes after %d inside %q", topic.ID, topic.order, lastOrder, topic.Group)
		}
		lastOrder = topic.order
	}
	if groups[0] != "Start here" {
		t.Errorf("the manual opens on %q — it must open on Start here", groups[0])
	}
	if groups[len(groups)-1] != "Instance administration" {
		t.Errorf("the manual ends on %q — administration is documented last (§25.4)", groups[len(groups)-1])
	}
	// The first chapter of the manual is the one a new reader needs.
	if m.Topics[0].ID != "first-deploy" {
		t.Errorf("first topic = %q, want first-deploy", m.Topics[0].ID)
	}
}

// The remaining refusals: each one is a way the corpus can be wrong that the
// boot must catch, so each one is worth a case of its own.
func TestMoreParseErrors(t *testing.T) {
	valid := func(body string) fstest.MapFS {
		return fstest.MapFS{"m/x.md": &fstest.MapFile{Data: []byte(body)}}
	}
	head := "---\nid: x\ntitle: T\ngroup: Start here\nsummary: S\n"

	t.Run("an empty directory is not a manual", func(t *testing.T) {
		if _, err := load(fstest.MapFS{}, "m", Validator{}); err == nil {
			t.Fatal("expected a failure")
		}
	})

	t.Run("a missing directory names itself", func(t *testing.T) {
		_, err := load(fstest.MapFS{"other/x.md": &fstest.MapFile{}}, "m", Validator{})
		if err == nil || !strings.Contains(err.Error(), "manual") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("non-markdown files are ignored", func(t *testing.T) {
		fsys := fstest.MapFS{
			"m/x.md":        &fstest.MapFile{Data: []byte(head + "---\n\nText.\n")},
			"m/notes.txt":   &fstest.MapFile{Data: []byte("not a chapter")},
			"m/sub/deep.md": &fstest.MapFile{Data: []byte("ignored, not walked")},
		}
		m, err := load(fsys, "m", Validator{})
		if err != nil || len(m.Topics) != 1 {
			t.Fatalf("topics = %v err = %v", m, err)
		}
	})

	t.Run("an unknown icon", func(t *testing.T) {
		_, err := load(valid(head+"icon: nope\n---\n"), "m", Validator{KnownIcon: func(string) bool { return false }})
		if err == nil || !strings.Contains(err.Error(), "does not ship") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a link with both a route and an href", func(t *testing.T) {
		body := head + "links:\n  - label: L\n    route: /apps\n    href: https://x.example\n---\n"
		_, err := load(valid(body), "m", Validator{})
		if err == nil || !strings.Contains(err.Error(), "both a route and an href") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a link with no label", func(t *testing.T) {
		_, err := load(valid(head+"links:\n  - route: /apps\n---\n"), "m", Validator{})
		if err == nil || !strings.Contains(err.Error(), "no label") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a route that is not declared", func(t *testing.T) {
		body := head + "links:\n  - label: L\n    route: /ghost\n---\n"
		_, err := load(valid(body), "m", Validator{KnownRoute: func(string) bool { return false }})
		if err == nil || !strings.Contains(err.Error(), "not a declared dashboard route") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a heading that slugs to nothing", func(t *testing.T) {
		_, err := load(valid(head+"---\n\n## ***\n\nText.\n"), "m", Validator{})
		if err == nil || !strings.Contains(err.Error(), "slugs to nothing") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a chapter with no prose at all", func(t *testing.T) {
		m, err := load(valid(head+"---\n"), "m", Validator{})
		if err != nil {
			t.Fatal(err)
		}
		if m.Topics[0].IntroHTML != "" || len(m.Topics[0].Sections) != 0 {
			t.Fatalf("topic = %+v", m.Topics[0])
		}
	})
}

// A warning must not render as a remark: the callout class is what keeps
// "this destroys data" from looking like "by the way".
func TestCalloutsAreDistinguishable(t *testing.T) {
	fsys := fstest.MapFS{"m/x.md": &fstest.MapFile{Data: []byte(`---
id: x
title: T
group: Start here
summary: S
---

## Callouts

> **Note** — a remark.

> **Warning** — this destroys data.

> An ordinary quotation.
`)}}
	m, err := load(fsys, "m", Validator{})
	if err != nil {
		t.Fatal(err)
	}
	html := m.Topics[0].Sections[0].HTML
	for _, want := range []string{`class="akd-note"`, `class="akd-warn"`} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %s in: %s", want, html)
		}
	}
	// An ordinary blockquote stays ordinary: the marker is the opening word,
	// not the shape.
	if got := strings.Count(html, "<blockquote"); got != 3 {
		t.Fatalf("blockquotes = %d", got)
	}
	if strings.Count(html, "class=") != 2 {
		t.Errorf("a plain quotation must carry no class: %s", html)
	}
}
