package cli

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
	"time"

	"github.com/spf13/cobra"
)

// previewCmd is the preview verb space of an application (ADR-070 §2): the
// reviewer's read path of ADR-059 answered from a terminal, plus the two acts
// that are part of debugging on a PR instance — redeploying it, and holding it
// alive while you work on it.
//
// **`approve` is deliberately absent.** The endpoint exists
// (POST …/previews/{uuid}/approve) and this group will not call it: authorizing
// a fork's preview to run is project governance (INV-010, §20.4.8), taken by a
// maintainer who is looking at the diff, and this CLI is a runtime and debugging
// tool. Its absence is a decision, which is why a test asserts it rather than
// leaving the next reader to assume it was an oversight.
func previewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "preview",
		Aliases: []string{"previews"},
		Short:   "PR previews of an application",
		Long: "The ephemeral environments an application deploys per pull request (§20.4). " +
			"`list` says which PR is up and where; `redeploy` reruns one at the SHA it is " +
			"already pinned to; `keep` re-arms its inactivity clock so it survives the " +
			"afternoon you spend debugging on it.\n\n" +
			"Approving a fork's preview is not here: it is a maintainer's call on a diff, " +
			"and it stays in the dashboard.",
	}
	cmd.AddCommand(previewListCmd(), previewRedeployCmd(), previewKeepCmd())
	return cmd
}

// previewRecord is the Preview schema as the API serves it. Every field is
// carried, including the ones no column shows, because `-o json` promises the
// API object and not the table's excerpt of it.
type previewRecord struct {
	Uuid              string     `json:"uuid"`
	PrID              int        `json:"pr_id"`
	Provider          string     `json:"provider,omitempty"`
	SourceBranch      string     `json:"source_branch"`
	HeadSha           string     `json:"head_sha"`
	IsFork            bool       `json:"is_fork"`
	ForkApproved      bool       `json:"fork_approved"`
	Fqdn              string     `json:"fqdn"`
	Status            string     `json:"status"`
	DeployRequestedAt *time.Time `json:"deploy_requested_at"`
	LastDeployedAt    *time.Time `json:"last_deployed_at"`
	CreatedAt         time.Time  `json:"created_at"`
}

func previewListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list [NAME]",
		Aliases: listAliases(),
		Short:   "List the PR previews of an application",
		Long: "The non-destroyed previews of an application, newest pull request first. " +
			"A preview stuck in `queued` with an unapproved fork as its source is not " +
			"broken — it is waiting for a maintainer, which the SOURCE column says and " +
			"the status alone does not.",
		Example: "  akerdock app preview list\n" +
			"  akerdock app preview list varuna\n" +
			"  akerdock app preview ls -o json",
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
			previews, err := c.listPreviews(cmd.Context(), res.Uuid)
			if err != nil {
				return err
			}
			// Ordering is the table's, not the payload's: `-o json` hands back what
			// the API sent, in the order it sent it.
			if flags.output == "json" {
				return printJSON(previews)
			}
			slices.SortFunc(previews, func(a, b previewRecord) int { return b.PrID - a.PrID })
			rows := make([][]string, 0, len(previews))
			for _, p := range previews {
				rows = append(rows, []string{
					"#" + strconv.Itoa(p.PrID), p.Status, previewURL(p), previewSource(p),
					whenOrDash(p.LastDeployedAt), p.Uuid,
				})
			}
			table([]string{"PR", "STATUS", "URL", "SOURCE", "LAST DEPLOY", "UUID"}, rows)
			return nil
		},
	}
}

// listPreviews reads an application's previews. No cursor: the endpoint returns
// the non-destroyed set in one object, and previews are bounded by the app's
// concurrency cap (§20.4.3) rather than by history.
func (c *Client) listPreviews(ctx context.Context, appUUID string) ([]previewRecord, error) {
	var page struct {
		Data []previewRecord `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/applications/"+appUUID+"/previews", nil, nil, &page); err != nil {
		return nil, err
	}
	return page.Data, nil
}

// previewURL rebuilds the address from the stored host. The API keeps the FQDN
// alone; the scheme is always https, since the proxy issues the certificate —
// the same assumption the dashboard's preview links are built on.
func previewURL(p previewRecord) string {
	if p.Fqdn == "" {
		return "-"
	}
	return "https://" + p.Fqdn
}

// previewSource names what is being previewed and, for a fork, whether it is
// allowed to run at all. There is no "kept" field to show in its place: the
// TTL that `keep` re-arms lives server-side and the Preview schema exposes
// neither the deadline nor the flag, so the column that would have shown it is
// the one that explains the other reason a preview sits idle.
func previewSource(p previewRecord) string {
	branch := p.SourceBranch
	if branch == "" {
		branch = "-"
	}
	switch {
	case p.IsFork && !p.ForkApproved:
		return branch + " (fork, unapproved)"
	case p.IsFork:
		return branch + " (fork)"
	default:
		return branch
	}
}

// whenOrDash renders a nullable timestamp in local time, or the dash that says
// "never" — an empty cell would read as a rendering failure.
func whenOrDash(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func previewRedeployCmd() *cobra.Command {
	var (
		pr           int
		forceRebuild bool
		skipBuild    bool
	)
	cmd := &cobra.Command{
		Use:   "redeploy [NAME]",
		Short: "Redeploy a PR preview at the SHA it is pinned to",
		Long: "Redeploys THIS instance at the head SHA it already carries: the pull request " +
			"is not re-read, so a commit pushed since will not appear, and a fork stays " +
			"subject to its approval.\n\n" +
			"`--skip-build` reruns the pipeline over the artifact already running " +
			"(ADR-048) — the way to apply a change to the preview's own variables " +
			"(INV-010) without paying for a build.",
		Example: "  akerdock app preview redeploy --pr 42\n" +
			"  akerdock app preview redeploy --pr 42 varuna --force-rebuild\n" +
			"  akerdock app preview redeploy --pr 42 --skip-build   # apply variables, no build",
		Args: targetArgs(kindApp),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requirePR(pr, "redeploy"); err != nil {
				return err
			}
			// The API refuses the pair with a 422; refusing it here makes it exit 2,
			// which is what "you typed two contradictory flags" deserves.
			if forceRebuild && skipBuild {
				return usageErrorf("--force-rebuild and --skip-build are mutually exclusive — one builds without cache, the other does not build at all")
			}
			c, err := newClient(flags.context)
			if err != nil {
				return err
			}
			preview, err := c.targetPreview(cmd.Context(), args, pr)
			if err != nil {
				return err
			}
			// Always a map, never a nil one: a nil body typed as a map still
			// marshals, and it would send the literal `null` the spec has no
			// schema for.
			body := map[string]bool{}
			if forceRebuild {
				body["force_rebuild"] = true
			}
			if skipBuild {
				body["skip_build"] = true
			}
			var accepted struct {
				DeploymentUuid string `json:"deployment_uuid"`
				StatusUrl      string `json:"status_url"`
				JobUuid        string `json:"job_uuid"`
			}
			if err := c.do(cmd.Context(), http.MethodPost, preview.path+"/redeploy", nil, body, &accepted); err != nil {
				return err
			}
			if flags.output == "json" {
				return printJSON(accepted)
			}
			if !flags.quiet {
				_, _ = fmt.Fprintf(os.Stdout, "preview of PR #%d redeploying — deployment %s\n", pr, accepted.DeploymentUuid)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.IntVar(&pr, "pr", 0, "PR number of the preview to redeploy (required)")
	f.BoolVar(&forceRebuild, "force-rebuild", false, "build without the cache")
	f.BoolVar(&skipBuild, "skip-build", false, "redeploy the running artifact with the current configuration; no clone, no build")
	return cmd
}

func previewKeepCmd() *cobra.Command {
	var pr int
	cmd := &cobra.Command{
		Use:   "keep [NAME]",
		Short: "Re-arm a PR preview's inactivity clock",
		Long: "Resets the TTL (§20.4.3) and clears the expiration warning, so the preview " +
			"is not reaped while you are still on it. It buys one window, not immunity: " +
			"the next TTL runs from now.",
		Example: "  akerdock app preview keep --pr 42\n" +
			"  akerdock app preview keep --pr 42 varuna",
		Args: targetArgs(kindApp),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requirePR(pr, "keep"); err != nil {
				return err
			}
			c, err := newClient(flags.context)
			if err != nil {
				return err
			}
			preview, err := c.targetPreview(cmd.Context(), args, pr)
			if err != nil {
				return err
			}
			// No body and no response: the endpoint takes neither a duration nor a
			// toggle — it re-arms the window the application configured.
			if err := c.do(cmd.Context(), http.MethodPost, preview.path+"/keep", nil, nil, nil); err != nil {
				return err
			}
			if !flags.quiet {
				_, _ = fmt.Fprintf(os.Stdout, "preview of PR #%d kept — its inactivity clock is re-armed\n", pr)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&pr, "pr", 0, "PR number of the preview to keep alive (required)")
	return cmd
}

// previewTarget is a resolved preview plus the path its verbs post to, so the
// two callers below cannot disagree about how that path is spelled.
type previewTarget struct {
	previewInfo
	path string
}

// targetPreview resolves the application (positional name or `.akerdock`
// default) and then the preview of that PR, reusing the shared resolver so the
// destroyed-preview refusal is worded once.
func (c *Client) targetPreview(ctx context.Context, args []string, pr int) (previewTarget, error) {
	res, err := c.target(ctx, kindApp, args)
	if err != nil {
		return previewTarget{}, err
	}
	info, err := c.resolvePreview(ctx, res.Uuid, pr)
	if err != nil {
		return previewTarget{}, err
	}
	return previewTarget{
		previewInfo: info,
		path:        "/applications/" + res.Uuid + "/previews/" + info.Uuid,
	}, nil
}

// requirePR refuses a preview verb that names no pull request, as a caller
// error rather than a 404. There is no directory default to fall back on: a
// repository declares the application it deploys, never the PR you happen to be
// debugging today.
func requirePR(pr int, verb string) error {
	if pr > 0 {
		return nil
	}
	return usageErrorf("usage: akerdock app preview %s --pr N [NAME]\n  example: akerdock app preview %s --pr 42", verb, verb)
}
