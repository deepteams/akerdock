package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// deployCmd is the deployment verb space of one kind (ADR-070 §2). A group
// rather than four top-level verbs because the history is consulted at least as
// often as a deployment is triggered, and `deploy list` standing next to
// `deploy run` says so in the help.
//
// `rollback` is mounted for applications only: the API has
// POST /applications/{uuid}/rollback and nothing equivalent for a compose
// stack. Offering the verb everywhere would move the refusal from `--help`,
// where it can be read before typing, to runtime — the exact failure ADR-070 §1
// set out to remove.
func deployCmd(k resourceKind) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deploy",
		Short: "Trigger a deployment of this " + k.label + ", and read its history",
		Long: "Triggers deployments and reads what they did. A deployment starts from a " +
			"source the platform can fetch again — a Git ref or an image (ADR-070 §3) — so " +
			"nothing here uploads anything from this machine.",
	}
	cmd.AddCommand(deployRunCmd(k), deployListCmd(k), deployCancelCmd())
	if k == kindApp {
		cmd.AddCommand(deployRollbackCmd(k))
	}
	return cmd
}

// deploymentAccepted is DeploymentAccepted, the 202 envelope every trigger in
// this file answers with — run, rollback and cancel alike. It is the whole
// schema, so `-o json` on those three prints the API object entire.
type deploymentAccepted struct {
	DeploymentUuid string  `json:"deployment_uuid"`
	StatusURL      string  `json:"status_url"`
	JobUuid        *string `json:"job_uuid"`
}

// deployment is the projection of the Deployment schema the table renders. It
// is deliberately NOT what `-o json` prints: see listDeployments.
type deployment struct {
	Uuid         string     `json:"uuid"`
	Status       string     `json:"status"`
	Trigger      string     `json:"trigger"`
	PrID         *int       `json:"pr_id"`
	Branch       string     `json:"branch"`
	IsRollback   bool       `json:"is_rollback"`
	SkipBuild    bool       `json:"skip_build"`
	ForceRebuild bool       `json:"force_rebuild"`
	CommitSha    string     `json:"commit_sha"`
	ImageDigest  string     `json:"image_digest"`
	StartedAt    *time.Time `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at"`
	CreatedAt    time.Time  `json:"created_at"`
}

// deployOptions is the request body of POST /applications/{uuid}/deploy. Both
// fields are optional server-side, so the body is sent only when the caller
// asked for one of them — an empty object and no body mean the same deployment.
type deployOptions struct {
	SkipBuild    bool `json:"skip_build,omitempty"`
	ForceRebuild bool `json:"force_rebuild,omitempty"`
}

func deployRunCmd(k resourceKind) *cobra.Command {
	var (
		skipBuild    bool
		forceRebuild bool
		follow       bool
	)
	cmd := &cobra.Command{
		Use:   "run [NAME]",
		Short: "Queue a deployment",
		Long: "Queues a deployment and prints the uuid that tracks it. With -f the build log " +
			"is streamed until the deployment reaches a terminal status; without it the " +
			"command returns as soon as the platform has accepted the work, which is what a " +
			"CI step wants.",
		Example: "  akerdock " + k.group + " deploy run\n" +
			"  akerdock " + k.group + " deploy run varuna -f\n" +
			"  akerdock " + k.group + " deploy run varuna --skip-build   # apply config, no build (ADR-048)",
		Args: targetArgs(k),
		RunE: func(cmd *cobra.Command, args []string) error {
			if skipBuild && forceRebuild {
				// The spec states the exclusivity; enforcing it here turns a 422
				// round trip into an answer typed before anything is queued.
				return usageErrorf("--skip-build and --force-rebuild are mutually exclusive — one builds nothing, the other builds everything")
			}
			c, err := newClient(flags.context)
			if err != nil {
				return err
			}
			res, err := c.target(cmd.Context(), k, args)
			if err != nil {
				return err
			}
			var body any
			if skipBuild || forceRebuild {
				body = deployOptions{SkipBuild: skipBuild, ForceRebuild: forceRebuild}
			}
			acc, err := c.postDeployment(cmd.Context(), k.path+"/"+res.Uuid+"/deploy", body)
			if err != nil {
				return err
			}
			return c.reportDeployment(cmd.Context(), res.Uuid, acc, follow)
		},
	}
	f := cmd.Flags()
	f.BoolVarP(&follow, "follow", "f", false, "stream the build log until the deployment ends")
	// Only the application trigger takes a body: POST /services/{uuid}/deploy
	// declares none (ADR-070 §1's asymmetry, in flags rather than verbs). A
	// --skip-build that the server silently drops would be worse than its
	// absence — the caller would believe a build was skipped.
	if k == kindApp {
		f.BoolVar(&skipBuild, "skip-build", false, "redeploy the running artifact with the current configuration — no clone, no build (ADR-048)")
		f.BoolVar(&forceRebuild, "force-rebuild", false, "build without the cache")
	}
	return cmd
}

// deployRollbackCmd redeploys an earlier artifact. Applications only — see
// deployCmd.
func deployRollbackCmd(k resourceKind) *cobra.Command {
	var (
		to     string
		follow bool
	)
	cmd := &cobra.Command{
		Use:   "rollback [NAME]",
		Short: "Redeploy a previous image",
		Long: "Redeploys an image that already ran. Without --to, the most recent previous " +
			"artifact still available. The image must still exist — retention or a pruned " +
			"registry can make an old deployment unrollbackable, and the platform answers so " +
			"rather than rebuilding it from source.\n\n" +
			"`akerdock " + k.group + " deploy list` prints the uuids --to accepts.",
		Example: "  akerdock " + k.group + " deploy rollback varuna\n" +
			"  akerdock " + k.group + " deploy rollback varuna --to 3f2b1c8e-…",
		Args: targetArgs(k),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(flags.context)
			if err != nil {
				return err
			}
			res, err := c.target(cmd.Context(), k, args)
			if err != nil {
				return err
			}
			// The endpoint also accepts an explicit image_digest. It is not
			// exposed: nothing in this CLI hands the operator a digest to paste,
			// while `deploy list` hands them a uuid on every row.
			var body any
			if to != "" {
				body = map[string]string{"deployment_uuid": to}
			}
			acc, err := c.postDeployment(cmd.Context(), k.path+"/"+res.Uuid+"/rollback", body)
			if err != nil {
				return err
			}
			return c.reportDeployment(cmd.Context(), res.Uuid, acc, follow)
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "uuid of the successful deployment whose image must be redeployed (default: the previous one)")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "stream the log until the rollback ends")
	return cmd
}

// deployCancelCmd takes a deployment uuid and no resource name: the endpoint is
// transversal (POST /deployments/{uuid}/cancel), a deployment uuid already
// identifies its application, and the group is only where a reader looks for it.
func deployCancelCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cancel DEPLOYMENT_UUID",
		Short: "Cancel a queued or running deployment",
		Long: "Removes the candidate before the traffic switchover. The healthy container " +
			"serving right now is never touched (INV-006), so cancelling is safe on a live " +
			"application. A deployment already terminal, or already switching, is refused.",
		Example: "  akerdock app deploy list varuna\n  akerdock app deploy cancel 3f2b1c8e-…",
		Args:    usageArgs(1, "<type> deploy cancel DEPLOYMENT_UUID", "app deploy cancel 3f2b1c8e-0000-4000-8000-000000000000"),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(flags.context)
			if err != nil {
				return err
			}
			acc, err := c.postDeployment(cmd.Context(), "/deployments/"+args[0]+"/cancel", nil)
			if err != nil {
				return err
			}
			if flags.output == "json" {
				return printJSON(acc)
			}
			if !flags.quiet {
				_, _ = fmt.Fprintln(os.Stdout, "cancellation requested for "+args[0])
			}
			return nil
		},
	}
}

func deployListCmd(k resourceKind) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:     "list [NAME]",
		Aliases: listAliases(),
		Short:   "Show the deployment history, newest first",
		Long: "The history is never rewritten: a retry is a new attempt, a superseded " +
			"deployment stays visible. Which is why the listing is bounded by -n rather " +
			"than walked whole.",
		Example: "  akerdock " + k.group + " deploy list varuna\n" +
			"  akerdock " + k.group + " deploy ls varuna -n 50",
		Args: targetArgs(k),
		RunE: func(cmd *cobra.Command, args []string) error {
			if limit <= 0 {
				return usageErrorf("--limit must be at least 1, got %d", limit)
			}
			c, err := newClient(flags.context)
			if err != nil {
				return err
			}
			res, err := c.target(cmd.Context(), k, args)
			if err != nil {
				return err
			}
			raw, err := c.listDeployments(cmd.Context(), k.path+"/"+res.Uuid+"/deployments", limit)
			if err != nil {
				return err
			}
			// -o json is what a script reads, and a script reads fields this table
			// has no column for — config_version, attempt, error_message, the
			// commit author. So the page is kept raw and only the table's
			// projection is decoded out of it.
			if flags.output == "json" {
				return printJSON(raw)
			}
			rows := make([][]string, 0, len(raw))
			for _, item := range raw {
				var d deployment
				if err := json.Unmarshal(item, &d); err != nil {
					return err
				}
				rows = append(rows, []string{
					d.Status, deployTrigger(d), deploySource(d),
					deployTime(d.StartedAt), deployTime(d.FinishedAt), d.Uuid,
				})
			}
			table([]string{"STATUS", "TRIGGER", "SOURCE", "STARTED", "FINISHED", "UUID"}, rows)
			return nil
		},
	}
	cmd.Flags().IntVarP(&limit, "limit", "n", 20, "how many deployments to show, newest first")
	return cmd
}

// postDeployment performs a trigger and decodes the 202 envelope. Every verb
// here answers with the same object, and every one of them is worth nothing
// without the uuid it carries.
func (c *Client) postDeployment(ctx context.Context, path string, body any) (deploymentAccepted, error) {
	var acc deploymentAccepted
	if err := c.do(ctx, http.MethodPost, path, nil, body, &acc); err != nil {
		return deploymentAccepted{}, err
	}
	return acc, nil
}

// reportDeployment announces the queued deployment and, with -f, follows its
// build log — the deployment the trigger JUST returned, never "the latest",
// which under a concurrent push would be someone else's.
//
// The announcement goes to stderr while following, so that `deploy run -f >
// build.log` keeps the log alone on stdout.
func (c *Client) reportDeployment(ctx context.Context, resourceUUID string, acc deploymentAccepted, follow bool) error {
	if flags.output == "json" && !follow {
		return printJSON(acc)
	}
	if !flags.quiet {
		out := os.Stdout
		if follow {
			out = os.Stderr
		}
		_, _ = fmt.Fprintf(out, "deployment %s queued\n", acc.DeploymentUuid)
	} else if !follow {
		// Quiet is the scripted form: the uuid alone, ready to be captured.
		_, _ = fmt.Fprintln(os.Stdout, acc.DeploymentUuid)
	}
	if !follow {
		return nil
	}
	return c.streamDeploymentLogs(ctx, resourceUUID, acc.DeploymentUuid)
}

// listDeployments walks the cursor pagination up to `limit` rows. The history
// of a busy application is unbounded, so the walk stops at what was asked for
// instead of at next_cursor == null.
func (c *Client) listDeployments(ctx context.Context, path string, limit int) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, 0, limit)
	cursor := ""
	for len(out) < limit {
		want := min(limit-len(out), 100)
		q := url.Values{"limit": {strconv.Itoa(want)}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var page struct {
			Data       []json.RawMessage `json:"data"`
			NextCursor *string           `json:"next_cursor"`
		}
		if err := c.do(ctx, http.MethodGet, path, q, nil, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Data...)
		// An empty page with a cursor would loop forever; the API does not
		// promise it cannot happen, and a CLI that hangs is worse than one that
		// shows a short page.
		if page.NextCursor == nil || *page.NextCursor == "" || len(page.Data) == 0 {
			break
		}
		cursor = *page.NextCursor
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// deployTrigger names who asked, plus the two booleans that change what the
// deployment DID without changing who asked: the schema says explicitly that a
// rollback is signalled by is_rollback and not by the trigger, and ADR-048's
// skip_build is invisible in it too. Both are the first thing wanted when a
// history reads oddly.
func deployTrigger(d deployment) string {
	switch {
	case d.PrID != nil:
		return fmt.Sprintf("%s #%d", d.Trigger, *d.PrID)
	case d.IsRollback:
		return d.Trigger + " (rollback)"
	case d.SkipBuild:
		return d.Trigger + " (no build)"
	case d.ForceRebuild:
		return d.Trigger + " (no cache)"
	}
	return d.Trigger
}

// deploySource answers "what is running because of this line": a commit for a
// git source, a digest for an image one. Nullable everywhere, because the same
// history holds both kinds.
func deploySource(d deployment) string {
	sha := shortHash(d.CommitSha, 7)
	switch {
	case d.Branch != "" && sha != "":
		return d.Branch + "@" + sha
	case sha != "":
		return sha
	case d.Branch != "":
		return d.Branch
	case d.ImageDigest != "":
		// The algorithm prefix is constant noise in a column read vertically.
		_, hash, found := strings.Cut(d.ImageDigest, ":")
		if !found {
			hash = d.ImageDigest
		}
		return shortHash(hash, 12)
	}
	return "-"
}

func shortHash(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// deployTime renders a nullable timestamp: a queued deployment has not started
// and a running one has not finished, and both are normal states to read.
func deployTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}
