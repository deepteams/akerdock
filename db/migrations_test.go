package db_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// An upgrade is a tag change (ADR-021): the new binary starts, applies its
// migrations, and the OLD binary may still be running for a few seconds — or a
// few minutes, on a rolling multi-instance deploy (§18.2). The schema must
// therefore stay compatible with version N-1 for the duration.
//
// That means migrations are EXPAND-ONLY: they may add, never take away. A
// dropped column, a renamed table or a narrowed type breaks the running binary
// instantly, and the failure lands on whoever happened to be deploying at that
// moment.
//
// Contracting changes (dropping the column nobody reads any more) belong to a
// LATER release, once no N-1 is left. This test enforces the rule mechanically,
// because it is exactly the kind of rule everyone agrees with and nobody
// remembers at 2am.
//
// The `-- +goose Down` half is exempt: a rollback deliberately undoes things,
// and it is run by an operator who knows what they are doing.

type rule struct {
	pattern *regexp.Regexp
	why     string
}

var forbidden = []rule{
	{
		pattern: regexp.MustCompile(`(?i)\bDROP\s+TABLE\b`),
		why:     "dropping a table breaks the previous version instantly — drop it in a later release, once no N-1 is running",
	},
	{
		pattern: regexp.MustCompile(`(?i)\bDROP\s+COLUMN\b`),
		why:     "dropping a column breaks any N-1 binary still SELECTing it — stop writing it first, drop it in a later release",
	},
	{
		pattern: regexp.MustCompile(`(?i)\bALTER\s+COLUMN\s+\w+\s+TYPE\b`),
		why:     "changing a column type rewrites values under the running binary — add a new column and migrate in two steps",
	},
	{
		pattern: regexp.MustCompile(`(?i)\bRENAME\s+(TO|COLUMN)\b`),
		why:     "a rename is a drop and an add at once — add the new name, backfill, remove the old one in a later release",
	},
	{
		pattern: regexp.MustCompile(`(?i)\bDROP\s+(TYPE|INDEX)\b`),
		why:     "dropping a type or index the previous version still depends on breaks it — postpone to a later release",
	},
}

// exempt lists migrations allowed to contain a contracting statement, keyed by
// filename, with the reason the expand-only rationale does not apply. The bar
// for an entry: the object being removed must never have shipped in a release
// (git tag), so that no N-1 binary can possibly be reading it. A table that
// only ever existed between two commits of the same unreleased line has no
// previous version to break.
var exempt = map[string]string{
	// role_assignments was created by 00081 (ADR-046) and withdrawn by 00082
	// (ADR-047) on the same unreleased mainline — no tagged release ever
	// carried the table, so no running binary can depend on it. Deferring the
	// drop would protect nobody.
	"00082_drop_scoped_role_assignments.sql": "table created by 00081 and withdrawn by 00082 (ADR-047) before any release",
}

// addColumn matches an ADD COLUMN and captures its definition, to check that a
// NOT NULL column arrives with a default: without one, the INSERTs of the still
// running N-1 binary — which knows nothing of the column — all fail.
var addColumn = regexp.MustCompile(`(?is)\bADD\s+COLUMN\s+(?:IF\s+NOT\s+EXISTS\s+)?(\w+)\s+([^,;]+)`)

func TestMigrationsAreExpandOnly(t *testing.T) {
	files, err := filepath.Glob("migrations/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("no migrations found: %v", err)
	}

	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			up := upSection(string(raw))

			if why, ok := exempt[filepath.Base(file)]; ok {
				t.Logf("contracting statements allowed: %s", why)
			} else {
				for _, rule := range forbidden {
					if loc := rule.pattern.FindString(up); loc != "" {
						t.Errorf("%q is not backward-compatible: %s", strings.TrimSpace(loc), rule.why)
					}
				}
			}

			for _, m := range addColumn.FindAllStringSubmatch(up, -1) {
				name, definition := m[1], strings.ToUpper(m[2])
				if strings.Contains(definition, "NOT NULL") &&
					!strings.Contains(definition, "DEFAULT") &&
					!strings.Contains(definition, "GENERATED") {
					t.Errorf("column %q is NOT NULL without a default: every INSERT from the still-running "+
						"previous version would fail, since it does not know the column exists", name)
				}
			}
		})
	}
}

// upSection returns everything between `-- +goose Up` and `-- +goose Down`.
func upSection(sql string) string {
	_, after, found := strings.Cut(sql, "-- +goose Up")
	if !found {
		return ""
	}
	before, _, _ := strings.Cut(after, "-- +goose Down")
	return before
}
