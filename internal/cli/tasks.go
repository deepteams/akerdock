package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// tasksCmd is the scheduled-task verb space of an application (ADR-070 §2):
// the crons it runs inside its own container (§19.2). `run` triggers one now,
// through the same path as the cron trigger — overlap policy included — because
// running a task on demand is how you find out why it fails, instead of waiting
// for tomorrow's occurrence to fail the same way.
func tasksCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "tasks",
		Aliases: []string{"task"},
		Short:   "Scheduled tasks of an application",
		Long: "The commands an application runs on a cron inside its container (§19.2). " +
			"`list` shows the schedule, the last run and the next one; `run` triggers one " +
			"immediately, exactly as the scheduler would.",
	}
	cmd.AddCommand(tasksListCmd(), tasksRunCmd())
	return cmd
}

// scheduledTask is the ScheduledTask schema. The policies and the timeout have
// no column — they are configuration, read from `-o json` when the question is
// about the definition rather than about what ran.
type scheduledTask struct {
	Uuid            string     `json:"uuid"`
	ApplicationUuid string     `json:"application_uuid,omitempty"`
	Name            string     `json:"name"`
	Command         string     `json:"command"`
	Container       string     `json:"container,omitempty"`
	CronExpression  string     `json:"cron_expression"`
	Timezone        string     `json:"timezone,omitempty"`
	Enabled         bool       `json:"enabled"`
	OverlapPolicy   string     `json:"overlap_policy,omitempty"`
	MissedRunPolicy string     `json:"missed_run_policy,omitempty"`
	TimeoutSeconds  int        `json:"timeout_seconds,omitempty"`
	NextRunAt       *time.Time `json:"next_run_at"`
	LastRunAt       *time.Time `json:"last_run_at"`
}

func tasksListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list [NAME]",
		Aliases: listAliases(),
		Short:   "List the scheduled tasks of an application",
		Example: "  akerdock app tasks list\n" +
			"  akerdock app tasks list varuna\n" +
			"  akerdock app tasks ls -o json    # the whole command, and the policies",
		Args: targetArgs(kindApp),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(flags.context)
			if err != nil {
				return err
			}
			res, err := c.target(cmd.Context(), kindApp, args)
			if err != nil {
				return err
			}
			tasks, err := c.listScheduledTasks(cmd.Context(), res.Uuid)
			if err != nil {
				return err
			}
			if flags.output == "json" {
				return printJSON(tasks)
			}
			rows := make([][]string, 0, len(tasks))
			for _, t := range tasks {
				rows = append(rows, []string{
					t.Name, taskSchedule(t), taskState(t),
					whenOrDash(t.LastRunAt), whenOrDash(t.NextRunAt),
					elide(t.Command, 48), t.Uuid,
				})
			}
			table([]string{"NAME", "SCHEDULE", "STATE", "LAST RUN", "NEXT RUN", "COMMAND", "UUID"}, rows)
			return nil
		},
	}
}

// listScheduledTasks follows the cursor pagination of the tasks endpoint.
func (c *Client) listScheduledTasks(ctx context.Context, appUUID string) ([]scheduledTask, error) {
	var out []scheduledTask
	cursor := ""
	for {
		q := url.Values{"limit": {"100"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var page struct {
			Data       []scheduledTask `json:"data"`
			NextCursor *string         `json:"next_cursor"`
		}
		if err := c.do(ctx, http.MethodGet, "/applications/"+appUUID+"/scheduled-tasks", q, nil, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Data...)
		if page.NextCursor == nil || *page.NextCursor == "" {
			return out, nil
		}
		cursor = *page.NextCursor
	}
}

// taskSchedule folds the timezone into the cron expression, because "0 3 * * *"
// answers nothing on its own — a nightly task that runs at 04:00 local is a
// timezone the reader has to be told about. UTC stays implicit: it is the
// default, and repeating it on every row buys nothing.
func taskSchedule(t scheduledTask) string {
	if t.Timezone != "" && t.Timezone != "UTC" {
		return t.CronExpression + " (" + t.Timezone + ")"
	}
	return t.CronExpression
}

// taskState is the one word that explains an empty NEXT RUN.
func taskState(t scheduledTask) string {
	if t.Enabled {
		return "enabled"
	}
	return "disabled"
}

// elide keeps a table one row per task. A task command is arbitrary shell and
// can be a paragraph; the full text is one `-o json` away, and the ellipsis
// says so rather than pretending the command ended there. Cut on runes, not
// bytes: a command is as likely to hold a UTF-8 path as anything else here.
func elide(s string, width int) string {
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	return string(r[:width-1]) + "…"
}

func tasksRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run TASK [NAME]",
		Short: "Trigger a scheduled task now",
		Long: "Runs a task immediately, through the scheduler's own path: the overlap " +
			"policy still applies, so a task whose previous execution is still running " +
			"may be refused or queued exactly as the cron would have been.\n\n" +
			"TASK is the task's name, or its UUID when two tasks share a name.",
		Example: "  akerdock app tasks run nightly-cleanup\n" +
			"  akerdock app tasks run nightly-cleanup varuna\n" +
			"  akerdock app tasks run 7f3c…-uuid",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) < 1 || len(args) > 2 {
				return usageErrorf("usage: akerdock app tasks run TASK [NAME]\n  example: akerdock app tasks run nightly-cleanup")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(flags.context)
			if err != nil {
				return err
			}
			// The application is the LAST positional and optional, as everywhere in
			// the tree (ADR-070 §1) — here it simply follows TASK.
			res, err := c.target(cmd.Context(), kindApp, args[1:])
			if err != nil {
				return err
			}
			tasks, err := c.listScheduledTasks(cmd.Context(), res.Uuid)
			if err != nil {
				return err
			}
			task, err := resolveTask(tasks, args[0])
			if err != nil {
				return err
			}
			// The trigger returns a job, not an execution, and the history has no
			// key tying an entry to the job that started it — so following the run
			// from here would mean guessing which execution is yours. The job UUID
			// is what the API actually hands back, and it is what we print.
			var accepted struct {
				JobUuid   string `json:"job_uuid"`
				StatusUrl string `json:"status_url"`
			}
			if err := c.do(cmd.Context(), http.MethodPost, "/scheduled-tasks/"+task.Uuid+"/run", nil, nil, &accepted); err != nil {
				return err
			}
			if flags.output == "json" {
				return printJSON(accepted)
			}
			if !flags.quiet {
				_, _ = fmt.Fprintf(os.Stdout, "task %q triggered — job %s\n", task.Name, accepted.JobUuid)
			}
			return nil
		},
	}
}

// resolveTask turns TASK into one task of this application by the rule
// resolveNamed applies to a resource: a UUID matches outright, a name matches
// when it is the only one. Task names are not unique per application in the
// API, so the ambiguous case is real — and it is answered with the UUIDs to
// choose between rather than by silently running the first match.
func resolveTask(tasks []scheduledTask, ref string) (scheduledTask, error) {
	var byName []scheduledTask
	for _, t := range tasks {
		if t.Uuid == ref {
			return t, nil
		}
		if t.Name == ref {
			byName = append(byName, t)
		}
	}
	switch len(byName) {
	case 1:
		return byName[0], nil
	case 0:
		return scheduledTask{}, fmt.Errorf("no scheduled task named %q on this application — see `akerdock app tasks list`", ref)
	default:
		uuids := make([]string, 0, len(byName))
		for _, t := range byName {
			uuids = append(uuids, t.Uuid)
		}
		return scheduledTask{}, fmt.Errorf("several scheduled tasks named %q — run it by UUID: %s", ref, strings.Join(uuids, ", "))
	}
}
