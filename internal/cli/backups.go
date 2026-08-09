package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"
)

// backupsCmd is the backup half of the database group (ADR-070 §2): read the
// plans and their history, and run one now.
//
// There is no `restore` and no `download`, for two different reasons that the
// help says out loud so neither reads as an oversight. `restore` is refused by
// decision — overwriting a production database is the one act in this group
// whose blast radius does not fit behind a one-line terminal confirmation, and
// it keeps the dashboard's context. `download` is absent because no endpoint
// serves the file: an execution exposes its filename, size and checksum and
// nothing that streams bytes (ADR-070 §1's gap list).
func backupsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backups",
		Short: "Backup plans of a database, and their executions",
		Long: "The plans configured on a database, the history each one produced, and the " +
			"trigger that runs one now.\n\n" +
			"Restoring is deliberately not here: it is done from the dashboard, where the " +
			"database being overwritten is named on the same screen as the file. Downloading " +
			"is not here either — no endpoint serves the backup file.",
	}
	cmd.AddCommand(backupsListCmd(), backupsRunCmd())
	return cmd
}

// backupPlan is the projection of BackupPlan the table renders. A plan has no
// name in the schema: `--plan` therefore takes the uuid, and every message that
// has to designate a plan prints the frequency next to it so the uuid can be
// recognised rather than merely copied.
type backupPlan struct {
	Uuid                string     `json:"uuid"`
	Frequency           string     `json:"frequency"`
	Timezone            string     `json:"timezone"`
	Enabled             bool       `json:"enabled"`
	SaveLocal           bool       `json:"save_local"`
	SaveS3              bool       `json:"save_s3"`
	S3Only              bool       `json:"s3_only"`
	NextRunAt           *time.Time `json:"next_run_at"`
	LastExecutionStatus string     `json:"last_execution_status"`
}

// backupExecution is the projection of BackupExecution. `partial` is a status
// of its own — a local success whose S3 upload failed — and the table shows it
// verbatim rather than folding it into success or failure (§20.5).
type backupExecution struct {
	Uuid       string     `json:"uuid"`
	PlanUuid   string     `json:"backup_plan_uuid"`
	Status     string     `json:"status"`
	Trigger    string     `json:"trigger"`
	Filename   string     `json:"filename"`
	SizeBytes  *int64     `json:"size_bytes"`
	S3Uploaded bool       `json:"s3_uploaded"`
	Message    string     `json:"message"`
	FinishedAt *time.Time `json:"finished_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

func backupsListCmd() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:     "list [NAME]",
		Aliases: listAliases(),
		Short:   "List the database's backup plans and their recent executions",
		Long: "A plan on its own says what is supposed to happen; the executions say what " +
			"did. The question asked of a backup is always the second one, so both are " +
			"printed together and the history is bounded by -n rather than walked whole.",
		Example: "  akerdock db backups list pg-main\n" +
			"  akerdock db backups ls pg-main -n 3\n" +
			"  akerdock db backups list pg-main -o json",
		Args: targetArgs(kindDB),
		RunE: func(cmd *cobra.Command, args []string) error {
			if limit <= 0 {
				return usageErrorf("--limit must be at least 1, got %d", limit)
			}
			c, err := newClient(flags.context)
			if err != nil {
				return err
			}
			res, err := c.target(cmd.Context(), kindDB, args)
			if err != nil {
				return err
			}
			plans, err := c.listBackupPlans(cmd.Context(), res.Uuid)
			if err != nil {
				return err
			}

			// The pages stay raw down to the projection: a script reading -o json
			// wants the retention rules, the drill results and the checksum, none
			// of which this table has a column for (same reasoning as `deploy
			// list`). The envelope only pairs each plan with its own history,
			// which no single endpoint returns.
			type planHistory struct {
				Plan       json.RawMessage   `json:"plan"`
				Executions []json.RawMessage `json:"executions"`
			}
			history := make([]planHistory, 0, len(plans))
			for _, raw := range plans {
				execs, err := c.listBackupExecutions(cmd.Context(), res.Uuid, rawPlanUUID(raw), limit)
				if err != nil {
					return err
				}
				history = append(history, planHistory{Plan: raw, Executions: execs})
			}
			if flags.output == "json" {
				return printJSON(history)
			}

			if len(plans) == 0 {
				if !flags.quiet {
					_, _ = fmt.Fprintf(os.Stdout, "%s has no backup plan\n", res.Name)
				}
				return nil
			}

			planRows := make([][]string, 0, len(history))
			execRows := make([][]string, 0, len(history)*limit)
			// The plan column only earns its width when there is more than one
			// plan to tell apart; on the common single-plan database it would be
			// the same uuid repeated down the page.
			multi := len(history) > 1
			for _, h := range history {
				var p backupPlan
				if err := json.Unmarshal(h.Plan, &p); err != nil {
					return err
				}
				planRows = append(planRows, []string{
					p.Frequency, backupEnabled(p), backupDestination(p),
					backupTime(p.NextRunAt), backupOrDash(p.LastExecutionStatus), p.Uuid,
				})
				for _, raw := range h.Executions {
					var e backupExecution
					if err := json.Unmarshal(raw, &e); err != nil {
						return err
					}
					row := []string{
						e.Status, backupOrDash(e.Trigger), backupSize(e.SizeBytes),
						backupWhen(e), backupOrDash(e.Filename), e.Uuid,
					}
					if multi {
						row = append([]string{p.Uuid}, row...)
					}
					execRows = append(execRows, row)
				}
			}
			table([]string{"FREQUENCY", "ENABLED", "DESTINATION", "NEXT RUN", "LAST", "UUID"}, planRows)
			if len(execRows) == 0 {
				return nil
			}
			_, _ = fmt.Fprintln(os.Stdout)
			header := []string{"STATUS", "TRIGGER", "SIZE", "WHEN", "FILE", "UUID"}
			if multi {
				header = append([]string{"PLAN"}, header...)
			}
			table(header, execRows)
			return nil
		},
	}
	cmd.Flags().IntVarP(&limit, "limit", "n", 5, "how many executions to show per plan, newest first")
	return cmd
}

func backupsRunCmd() *cobra.Command {
	var plan string
	cmd := &cobra.Command{
		Use:   "run [NAME]",
		Short: "Run one of the database's backup plans now",
		Long: "Runs a plan outside its schedule. The API allows a single execution per plan " +
			"at a time, so a plan already running is refused rather than queued twice.",
		Example: "  akerdock db backups run pg-main\n" +
			"  akerdock db backups run pg-main --plan 3f2b1c8e-…",
		Args: targetArgs(kindDB),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(flags.context)
			if err != nil {
				return err
			}
			res, err := c.target(cmd.Context(), kindDB, args)
			if err != nil {
				return err
			}
			raws, err := c.listBackupPlans(cmd.Context(), res.Uuid)
			if err != nil {
				return err
			}
			plans := make([]backupPlan, 0, len(raws))
			for _, raw := range raws {
				var p backupPlan
				if err := json.Unmarshal(raw, &p); err != nil {
					return err
				}
				plans = append(plans, p)
			}
			chosen, err := choosePlan(res.Name, plans, plan)
			if err != nil {
				return err
			}
			var acc struct {
				JobUuid       string `json:"job_uuid"`
				StatusURL     string `json:"status_url"`
				ExecutionUuid string `json:"execution_uuid"`
			}
			path := kindDB.path + "/" + res.Uuid + "/backups/" + chosen.Uuid + "/execute"
			if err := c.do(cmd.Context(), http.MethodPost, path, nil, nil, &acc); err != nil {
				return err
			}
			if flags.output == "json" {
				return printJSON(acc)
			}
			if flags.quiet {
				// The scripted form: the execution uuid alone, ready to be captured
				// and passed back to `backups list`.
				_, _ = fmt.Fprintln(os.Stdout, acc.ExecutionUuid)
				return nil
			}
			_, _ = fmt.Fprintf(os.Stdout, "backup started on %s — execution %s\n", res.Name, acc.ExecutionUuid)
			return nil
		},
	}
	cmd.Flags().StringVar(&plan, "plan", "", "uuid of the plan to run (required when the database has several)")
	return cmd
}

// choosePlan resolves which plan `run` triggers.
//
// With several plans and no --plan, it refuses instead of picking one: the
// plans differ in destination and retention, and running "a" backup when the
// caller meant the S3 one produces a file nobody will look for. The refusal
// prints the plans it is choosing between, so the answer is on screen rather
// than one `backups list` away.
func choosePlan(dbName string, plans []backupPlan, want string) (backupPlan, error) {
	if want != "" {
		for _, p := range plans {
			if p.Uuid == want {
				return p, nil
			}
		}
		return backupPlan{}, usageErrorf("no backup plan %q on %s — its plans are:\n%s", want, dbName, planChoices(plans))
	}
	switch len(plans) {
	case 0:
		// Not the caller's spelling: the database is configured this way.
		return backupPlan{}, fmt.Errorf("%s has no backup plan — create one from the dashboard first", dbName)
	case 1:
		return plans[0], nil
	default:
		return backupPlan{}, usageErrorf("%s has %d backup plans — name one with --plan:\n%s", dbName, len(plans), planChoices(plans))
	}
}

// planChoices lists the plans as the flag expects them: the uuid to paste,
// with the frequency and destination that make it recognisable.
func planChoices(plans []backupPlan) string {
	out := ""
	for _, p := range plans {
		out += fmt.Sprintf("  --plan %s  (%s, %s, %s)\n", p.Uuid, p.Frequency, backupDestination(p), backupEnabled(p))
	}
	return out
}

// rawPlanUUID pulls the one field the raw page is walked for. The plan objects
// stay raw for -o json; only the uuid is needed to fetch their history.
func rawPlanUUID(raw json.RawMessage) string {
	var p struct {
		Uuid string `json:"uuid"`
	}
	_ = json.Unmarshal(raw, &p)
	return p.Uuid
}

// listBackupPlans walks GET /databases/{uuid}/backups. Plans are few by
// construction, so this one follows the cursor to the end.
func (c *Client) listBackupPlans(ctx context.Context, dbUUID string) ([]json.RawMessage, error) {
	var out []json.RawMessage
	cursor := ""
	for {
		q := url.Values{"limit": {"100"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var page struct {
			Data       []json.RawMessage `json:"data"`
			NextCursor *string           `json:"next_cursor"`
		}
		if err := c.do(ctx, http.MethodGet, kindDB.path+"/"+dbUUID+"/backups", q, nil, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Data...)
		if page.NextCursor == nil || *page.NextCursor == "" || len(page.Data) == 0 {
			return out, nil
		}
		cursor = *page.NextCursor
	}
}

// listBackupExecutions reads one bounded page of the history, newest first as
// the endpoint returns it. Unbounded: a daily plan kept for a year is 365 rows
// nobody asked for.
func (c *Client) listBackupExecutions(ctx context.Context, dbUUID, planUUID string, limit int) ([]json.RawMessage, error) {
	q := url.Values{"limit": {strconv.Itoa(min(limit, 100))}}
	var page struct {
		Data []json.RawMessage `json:"data"`
	}
	path := kindDB.path + "/" + dbUUID + "/backups/" + planUUID + "/executions"
	if err := c.do(ctx, http.MethodGet, path, q, nil, &page); err != nil {
		return nil, err
	}
	if len(page.Data) > limit {
		page.Data = page.Data[:limit]
	}
	return page.Data, nil
}

// backupDestination folds the three booleans that decide where the dump lands
// into the word an operator checks: a plan saving nowhere off-server is the one
// worth spotting in a listing.
func backupDestination(p backupPlan) string {
	switch {
	case p.S3Only:
		return "s3 only"
	case p.SaveLocal && p.SaveS3:
		return "local+s3"
	case p.SaveS3:
		return "s3"
	case p.SaveLocal:
		return "local"
	}
	return "-"
}

func backupEnabled(p backupPlan) string {
	if p.Enabled {
		return "enabled"
	}
	return "disabled"
}

// backupWhen prefers the moment the execution ended; a queued or running one
// has not, and its creation time is what the reader is looking for instead.
func backupWhen(e backupExecution) string {
	if e.FinishedAt != nil && !e.FinishedAt.IsZero() {
		return backupTime(e.FinishedAt)
	}
	return backupTime(&e.CreatedAt)
}

func backupTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}

// backupSize renders the file size in the unit a human compares: "is today's
// dump the size of yesterday's" is the check this column exists for, and it is
// not made in bytes.
func backupSize(n *int64) string {
	if n == nil {
		return "-"
	}
	size := float64(*n)
	units := []string{"B", "KB", "MB", "GB", "TB"}
	i := 0
	for size >= 1024 && i < len(units)-1 {
		size /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", *n)
	}
	return fmt.Sprintf("%.1f %s", size, units[i])
}

// backupOrDash keeps an empty optional field from opening a hole in the table
// (namespaced because the package has one of these per group).
func backupOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
