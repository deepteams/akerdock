package jobs

import "testing"

func TestSharedInterpolate(t *testing.T) {
	s := sharedEnv{refs: map[string]string{
		"team.REGION":     "eu-west",
		"project.DB_HOST": "db.internal",
	}}
	cases := map[string]string{
		"static":                          "static",
		"{{team.REGION}}":                 "eu-west",
		"pg://{{project.DB_HOST}}/x":      "pg://db.internal/x",
		"{{team.REGION}}-{{team.REGION}}": "eu-west-eu-west",
		// An unknown reference stays VERBATIM: a visible placeholder in the
		// container beats a silently empty value nobody can explain.
		"{{team.MISSING}}":     "{{team.MISSING}}",
		"{{environment.NOPE}}": "{{environment.NOPE}}",
		"{{server.NEVER_REF}}": "{{server.NEVER_REF}}", // server vars inject, they are not references
		"{{team.bad-key}}":     "{{team.bad-key}}",     // outside the key grammar
		"$VAR and ${OTHER} {{": "$VAR and ${OTHER} {{", // shell syntax untouched
	}
	for in, want := range cases {
		if got := s.interpolate(in); got != want {
			t.Fatalf("interpolate(%q) = %q, want %q", in, got, want)
		}
	}
	// Empty ref set: values pass through without regex work.
	empty := sharedEnv{}
	if got := empty.interpolate("{{team.REGION}}"); got != "{{team.REGION}}" {
		t.Fatalf("empty env must pass through: %q", got)
	}
}
