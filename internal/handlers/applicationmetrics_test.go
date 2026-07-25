package handlers

import "testing"

func TestParseDockerSize(t *testing.T) {
	cases := map[string]int64{
		"25MiB":  25 * 1024 * 1024,
		"512KiB": 512 * 1024,
		"2GB":    2_000_000_000,
		"900B":   900,
	}
	for in, want := range cases {
		got := parseDockerSize(in)
		if got == nil || *got != want {
			t.Errorf("parseDockerSize(%q) = %v, want %d", in, got, want)
		}
	}
	// Fractional binary size: truncation matches the handler's int64(v*mult).
	factor := 1.944 // a variable, so the conversion truncates at runtime
	wantGiB := int64(factor * float64(int64(1)<<30))
	if got := parseDockerSize("1.944GiB"); got == nil || *got != wantGiB {
		t.Errorf("parseDockerSize(1.944GiB) = %v, want %d", got, wantGiB)
	}
	if parseDockerSize("") != nil || parseDockerSize("nonsense") != nil {
		t.Error("expected nil for empty/unparseable size")
	}
}

func TestParsePercent(t *testing.T) {
	if v := parsePercent("12.34%"); v == nil || *v != 12.34 {
		t.Errorf("parsePercent(12.34%%) = %v", v)
	}
	if v := parsePercent(" 0.00% "); v == nil || *v != 0 {
		t.Errorf("parsePercent trims spaces: %v", v)
	}
	if parsePercent("--") != nil {
		t.Error("expected nil for unparseable percent")
	}
}

func TestParseDockerStats(t *testing.T) {
	// Two JSON rows as `docker stats --format '{{json .}}'` emits them.
	stdout := `{"Name":"abc-web","CPUPerc":"12.50%","MemUsage":"50MiB / 1GiB","MemPerc":"5.00%"}
{"Name":"abc-postgres","CPUPerc":"1.00%","MemUsage":"128MiB / 1GiB","MemPerc":"12.50%"}
garbage line`
	byName := parseDockerStats(stdout)
	if len(byName) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(byName))
	}
	web := byName["abc-web"]
	if web.cpu == nil || *web.cpu != 12.5 {
		t.Errorf("web cpu = %v", web.cpu)
	}
	if web.memBytes == nil || *web.memBytes != 50*1024*1024 {
		t.Errorf("web mem = %v", web.memBytes)
	}
	if _, ok := byName["abc-postgres"]; !ok {
		t.Error("postgres row missing")
	}
}
