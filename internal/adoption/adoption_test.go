package adoption

import (
	"encoding/json"
	"strings"
	"testing"
)

// inspect decodes a fixture; ~ stands for the backtick Traefik rules use
// (a raw Go string cannot contain one).
func inspect(t *testing.T, raw string) Inspect {
	t.Helper()
	var c Inspect
	if err := json.Unmarshal([]byte(strings.ReplaceAll(raw, "~", "`")), &c); err != nil {
		t.Fatalf("inspect fixture: %v", err)
	}
	return c
}

func TestManagedBoundary(t *testing.T) {
	proxy := inspect(t, `{"Id":"aaa","Name":"/akerdock-proxy","Config":{"Labels":{"akerdock.type":"proxy"}}}`)
	liveManaged := inspect(t, `{"Id":"bbb","Name":"/x","Config":{"Labels":{"akerdock.managed":"true","akerdock.resource_uuid":"11111111-1111-4111-8111-111111111111"}}}`)
	disowned := inspect(t, `{"Id":"ccc","Name":"/y","State":{"Status":"running"},"Config":{"Image":"nginx:1.25","Labels":{"akerdock.managed":"true","akerdock.resource_uuid":"22222222-2222-4222-8222-222222222222"}}}`)
	plain := inspect(t, `{"Id":"ddd","Name":"/z","State":{"Status":"running"},"Config":{"Image":"nginx:1.25","Labels":{}}}`)
	// akerdock-labelled but without a resource uuid: never touched.
	weird := inspect(t, `{"Id":"eee","Name":"/w","Config":{"Labels":{"akerdock.oneshot":"true"}}}`)

	out := BuildCandidates(ScanInput{
		Containers:        []Inspect{proxy, liveManaged, disowned, plain, weird},
		LiveResourceUUIDs: map[string]bool{"11111111-1111-4111-8111-111111111111": true},
	})
	if len(out) != 2 {
		t.Fatalf("expected 2 candidates (disowned + plain), got %d: %+v", len(out), out)
	}
	ids := map[string]bool{out[0].ID: true, out[1].ID: true}
	if !ids["ccc"] || !ids["ddd"] {
		t.Fatalf("expected the disowned and plain containers, got %v", ids)
	}
}

func TestContainerCandidateMapping(t *testing.T) {
	c := inspect(t, `{
		"Id":"0123456789abcdef","Name":"/my_App.1","State":{"Status":"running"},
		"Config":{
			"Image":"ghcr.io/acme/web:2.1",
			"Env":["PATH=/usr/bin","APP_SECRET=s3cret","PORT=8080"],
			"Labels":{"traefik.http.routers.web.rule":"Host(~app.example.com~) || Host(~www.example.com~)"},
			"ExposedPorts":{"8080/tcp":{},"9090/udp":{}}
		},
		"HostConfig":{"PortBindings":{"8080/tcp":[{"HostPort":"80"}]}},
		"Mounts":[
			{"Type":"volume","Name":"web_data","Destination":"/data"},
			{"Type":"bind","Source":"/srv/uploads","Destination":"/uploads"}
		],
		"NetworkSettings":{"Networks":{"bridge":{"IPAddress":"172.17.0.2"}}}
	}`)
	out := BuildCandidates(ScanInput{
		Containers: []Inspect{c},
		ImageEnv:   map[string][]string{"ghcr.io/acme/web:2.1": {"PATH=/usr/bin", "PORT=3000"}},
	})
	if len(out) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(out))
	}
	cand := out[0]
	if !cand.Adoptable || cand.Kind != "container" || cand.ProposedResourceType != "application" {
		t.Fatalf("unexpected candidate: %+v", cand)
	}
	if cand.ID != "0123456789ab" {
		t.Fatalf("candidate id must be the short container id, got %q", cand.ID)
	}
	if cand.ProposedName != "my-app-1" {
		t.Fatalf("proposed name not sanitized: %q", cand.ProposedName)
	}
	cc := cand.Containers[0]
	// PATH matches the image default (dropped); PORT differs (kept);
	// APP_SECRET is container-only (kept). Values never appear.
	if len(cc.EnvKeys) != 2 || cc.EnvKeys[0] != "APP_SECRET" || cc.EnvKeys[1] != "PORT" {
		t.Fatalf("env keys: %v", cc.EnvKeys)
	}
	raw, _ := json.Marshal(cand)
	if strings.Contains(string(raw), "s3cret") {
		t.Fatal("a variable VALUE leaked into the scan candidate (INV-003)")
	}
	if len(cc.Ports) != 2 || cc.Ports[0].ContainerPort != 8080 || cc.Ports[0].HostPort == nil || *cc.Ports[0].HostPort != 80 {
		t.Fatalf("ports: %+v", cc.Ports)
	}
	if len(cc.Mounts) != 2 || cc.Mounts[0].Kind != "volume" || cc.Mounts[0].Source != "web_data" || cc.Mounts[1].Kind != "bind" {
		t.Fatalf("mounts: %+v", cc.Mounts)
	}
	if len(cc.Domains) != 2 || cc.Domains[0] != "app.example.com" || cc.Domains[1] != "www.example.com" {
		t.Fatalf("domains: %v", cc.Domains)
	}
}

func TestContainerBlockers(t *testing.T) {
	c := inspect(t, `{"Id":"fff","Name":"/priv","State":{"Status":"running"},
		"Config":{"Image":"nginx","Labels":{}},
		"HostConfig":{"Privileged":true,"CapAdd":["NET_ADMIN"]}}`)
	out := BuildCandidates(ScanInput{Containers: []Inspect{c}})
	if len(out) != 1 || out[0].Adoptable {
		t.Fatalf("a privileged container must not be adoptable: %+v", out)
	}
	if len(out[0].Reasons) < 2 {
		t.Fatalf("every blocker must be named (§20.7), got %v", out[0].Reasons)
	}
}

func TestStackCandidate(t *testing.T) {
	web := inspect(t, `{"Id":"a1","Name":"/shop-web-1","State":{"Status":"running"},
		"Config":{"Image":"nginx:1.25","Labels":{
			"com.docker.compose.project":"shop",
			"com.docker.compose.service":"web",
			"com.docker.compose.project.working_dir":"/opt/shop",
			"traefik.http.routers.shop.rule":"Host(~shop.example.com~)"}}}`)
	db := inspect(t, `{"Id":"a2","Name":"/shop-db-1","State":{"Status":"running"},
		"Config":{"Image":"postgres:16","Labels":{
			"com.docker.compose.project":"shop",
			"com.docker.compose.service":"db"}}}`)
	compose := `services:
  web:
    image: nginx:1.25
  db:
    image: postgres:16
    volumes:
      - pgdata:/var/lib/postgresql/data
volumes:
  pgdata:
`
	out := BuildCandidates(ScanInput{
		Containers:   []Inspect{web, db},
		ComposeFiles: map[string]ComposeFile{"shop": {Content: compose}},
	})
	if len(out) != 1 {
		t.Fatalf("expected one stack candidate, got %d", len(out))
	}
	cand := out[0]
	if cand.ID != "compose:shop" || cand.Kind != "compose_stack" || !cand.Adoptable {
		t.Fatalf("unexpected stack candidate: %+v", cand)
	}
	if cand.ProposedResourceType != "service" || cand.ComposeProject != "shop" {
		t.Fatalf("stack mapping: %+v", cand)
	}
	if len(cand.Containers) != 2 || cand.Containers[0].ComposeService != "db" && cand.Containers[1].ComposeService != "db" {
		t.Fatalf("stack members: %+v", cand.Containers)
	}
	// The stored compose pins pgdata to its current docker name.
	if !strings.Contains(cand.ComposeContent, "external: true") || !strings.Contains(cand.ComposeContent, "shop_pgdata") {
		t.Fatalf("volumes not pinned external:\n%s", cand.ComposeContent)
	}
}

func TestStackWithoutComposeFileIsNotAdoptable(t *testing.T) {
	web := inspect(t, `{"Id":"b1","Name":"/x-web-1","State":{"Status":"running"},
		"Config":{"Image":"nginx","Labels":{"com.docker.compose.project":"x","com.docker.compose.service":"web"}}}`)
	out := BuildCandidates(ScanInput{
		Containers:   []Inspect{web},
		ComposeFiles: map[string]ComposeFile{"x": {}},
	})
	if out[0].Adoptable {
		t.Fatal("a stack without its compose file must not be adoptable")
	}
	if len(out[0].Reasons) == 0 {
		t.Fatal("the reason must be named (§20.7)")
	}
}

func TestRewriteComposeExternalVolumes(t *testing.T) {
	in := `services:
  db:
    image: postgres:16
    volumes:
      - data:/var/lib/postgresql/data
volumes:
  data:
  named:
    name: custom_name
  already:
    external: true
`
	out, err := RewriteComposeExternalVolumes(in, "proj")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"external: true", "name: proj_data", "name: custom_name"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "name: proj_already") {
		t.Fatalf("an already-external volume must stay untouched:\n%s", out)
	}
	// The services section is preserved verbatim in meaning.
	if !strings.Contains(out, "data:/var/lib/postgresql/data") {
		t.Fatalf("service mounts must survive the rewrite:\n%s", out)
	}
}

func TestRewriteComposeNoVolumes(t *testing.T) {
	in := "services:\n  web:\n    image: nginx\n"
	out, err := RewriteComposeExternalVolumes(in, "p")
	if err != nil || out != in {
		t.Fatalf("a file without named volumes must pass through unchanged (err=%v):\n%s", err, out)
	}
}

func TestPointer(t *testing.T) {
	if ParsePointer(nil) != nil || ParsePointer([]byte("null")) != nil {
		t.Fatal("empty adoption column must parse to nil")
	}
	raw := []byte(`{"container_name":"legacy-web","scan_uuid":"s"}`)
	if got := ContainerName(raw, "uuid"); got != "legacy-web" {
		t.Fatalf("ContainerName = %q", got)
	}
	if got := ContainerName(nil, "uuid"); got != "uuid" {
		t.Fatalf("fallback = %q", got)
	}
}

func TestAdoptedEnv(t *testing.T) {
	env := []string{"PATH=/usr/bin", "PORT=8080", "SECRET=x", "EMPTY"}
	defaults := []string{"PATH=/usr/bin", "PORT=3000"}
	got := AdoptedEnv(env, defaults)
	if len(got) != 2 || got["PORT"] != "8080" || got["SECRET"] != "x" {
		t.Fatalf("AdoptedEnv = %v", got)
	}
	if all := AdoptedEnv(env, nil); len(all) != 3 {
		t.Fatalf("without image defaults everything is adopted: %v", all)
	}
}

func TestSplitImageRef(t *testing.T) {
	cases := []struct{ ref, name, tag string }{
		{"nginx", "nginx", ""},
		{"nginx:1.25", "nginx", "1.25"},
		{"registry.example.com:5000/acme/web:2.1", "registry.example.com:5000/acme/web", "2.1"},
		{"registry.example.com:5000/acme/web", "registry.example.com:5000/acme/web", ""},
		{"nginx@sha256:abc", "nginx@sha256:abc", ""},
	}
	for _, c := range cases {
		name, tag := SplitImageRef(c.ref)
		if name != c.name || tag != c.tag {
			t.Fatalf("SplitImageRef(%q) = %q,%q want %q,%q", c.ref, name, tag, c.name, c.tag)
		}
	}
}

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"My_App.1":              "my-app-1",
		"---":                   "adopted",
		"ok-name":               "ok-name",
		"UPPER":                 "upper",
		"a b\tc":                "a-b-c",
		strings.Repeat("x", 80): strings.Repeat("x", 63),
	}
	for in, want := range cases {
		if got := SanitizeName(in); got != want {
			t.Fatalf("SanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDomainsFromLabels(t *testing.T) {
	bt := func(s string) string { return strings.ReplaceAll(s, "~", "`") }
	labels := map[string]string{
		"traefik.http.routers.a.rule": bt("Host(~a.example.com~)"),
		"traefik.http.routers.b.rule": bt("PathPrefix(~/x~) && Host(~b.example.com~, ~c.example.com~)"),
		"caddy":                       "https://d.example.com",
		"unrelated":                   bt("Host(~nope.example.com~)"),
	}
	got := DomainsFromLabels(labels)
	want := []string{"a.example.com", "b.example.com", "c.example.com", "d.example.com"}
	if len(got) != len(want) {
		t.Fatalf("domains = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("domains = %v, want %v", got, want)
		}
	}
}
