package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type logLine struct {
	Sequence  int    `json:"sequence"`
	Timestamp string `json:"timestamp"`
	Channel   string `json:"channel"`
	Message   string `json:"message"`
}

// logsCmd reads the container logs of one kind. Mounted on the app group only:
// no other type has a logs endpoint (ADR-070 §1).
func logsCmd(k resourceKind) *cobra.Command {
	var (
		component  string
		lines      int
		follow     bool
		deployment string
		deployFlag bool
		pr         int
	)
	cmd := &cobra.Command{
		Use:   "logs [NAME]",
		Short: "Show container logs (snapshot or -f), or a deployment's logs",
		Example: "  akerdock app logs varuna\n" +
			"  akerdock app logs -f -c postgres        # default app from .akerdock\n" +
			"  akerdock app logs varuna --pr 42        # the PR #42 preview instance\n" +
			"  akerdock app logs varuna --pr 42 --deployment",
		Args: targetArgs(k),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(flags.context)
			if err != nil {
				return err
			}
			component = defaultComponent(component)
			res, err := c.target(cmd.Context(), k, args)
			if err != nil {
				return err
			}
			// --pr targets the PR instance instead of production (INV-011): its
			// containers, its build. Same resolution as `db --pr`.
			preview := previewInfo{}
			if pr > 0 {
				if preview, err = c.resolvePreview(cmd.Context(), res.Uuid, pr); err != nil {
					return err
				}
			}

			// Deployment logs: SSE stream (build), resumable.
			if deployFlag {
				if pr > 0 && (deployment == "" || deployment == "latest") {
					if deployment, err = c.latestPreviewDeployment(cmd.Context(), res.Uuid, pr); err != nil {
						return err
					}
				}
				return c.streamDeploymentLogs(cmd.Context(), res.Uuid, deployment)
			}

			q := url.Values{}
			if component != "" {
				q.Set("component", component)
			}
			// A preview's runtime logs are read on demand — the API offers no
			// stream for them, so -f polls and prints only what is new.
			if pr > 0 {
				path := "/applications/" + res.Uuid + "/previews/" + preview.Uuid + "/logs"
				q.Set("lines", strconv.Itoa(lines))
				if follow {
					return c.followSnapshotLogs(cmd.Context(), path, q)
				}
				return c.printSnapshotLogs(cmd.Context(), path, q)
			}
			if follow {
				return c.streamSSE(cmd.Context(), "/applications/"+res.Uuid+"/logs/stream", q, printLog)
			}
			q.Set("lines", strconv.Itoa(lines))
			return c.printSnapshotLogs(cmd.Context(), "/applications/"+res.Uuid+"/logs", q)
		},
	}
	f := cmd.Flags()
	f.StringVarP(&component, "component", "c", "", "compose service to read logs from")
	f.IntVarP(&lines, "lines", "n", 200, "number of lines (snapshot)")
	f.BoolVarP(&follow, "follow", "f", false, "stream logs as they arrive")
	f.StringVar(&deployment, "deployment", "", "read a deployment's logs (empty = latest)")
	f.IntVar(&pr, "pr", 0, "read the preview of this PR number instead of production")
	cmd.Flags().Lookup("deployment").NoOptDefVal = "latest"
	cmd.PreRun = func(cmd *cobra.Command, _ []string) {
		deployFlag = cmd.Flags().Changed("deployment")
	}
	return cmd
}

// printSnapshotLogs fetches one page of container logs and prints it.
func (c *Client) printSnapshotLogs(ctx context.Context, path string, query url.Values) error {
	var page struct {
		Data []logLine `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, path, query, nil, &page); err != nil {
		return err
	}
	if flags.output == "json" {
		return printJSON(page.Data)
	}
	for _, l := range page.Data {
		printLog(l)
	}
	return nil
}

// followSnapshotLogs emulates -f over an endpoint that only offers snapshots
// (the preview logs): it repolls and prints what the previous window did not
// already contain. `docker logs --tail` returns a sliding window with no
// stable identity per line — `sequence` is recomputed at each call — so the
// overlap is found on the content itself (newLogLines).
func (c *Client) followSnapshotLogs(ctx context.Context, path string, query url.Values) error {
	var previous []string
	first := true
	for {
		var page struct {
			Data []logLine `json:"data"`
		}
		if err := c.do(ctx, http.MethodGet, path, query, nil, &page); err != nil {
			return err
		}
		current := make([]string, 0, len(page.Data))
		for _, l := range page.Data {
			current = append(current, l.Message)
		}
		seen := 0
		if !first {
			seen = alreadySeenLines(previous, current)
		}
		for _, l := range page.Data[seen:] {
			printLog(l)
		}
		previous, first = current, false

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(3 * time.Second):
		}
	}
}

// alreadySeenLines counts how many lines at the head of `current` were already
// printed: the longest suffix of `previous` that is a prefix of `current`. No
// overlap at all means the window moved past everything we had seen, so the
// whole page is new — reprinting a line is a cosmetic fault, dropping one is a
// real loss.
func alreadySeenLines(previous, current []string) int {
	for k := min(len(previous), len(current)); k > 0; k-- {
		if slices.Equal(previous[len(previous)-k:], current[:k]) {
			return k
		}
	}
	return 0
}

// latestPreviewDeployment returns the uuid of the most recent deployment of one
// PR instance. The history is not filterable by preview server-side, but every
// deployment carries its pr_id — so the page is scanned rather than the whole
// history walked.
func (c *Client) latestPreviewDeployment(ctx context.Context, appUUID string, pr int) (string, error) {
	var page struct {
		Data []struct {
			Uuid string `json:"uuid"`
			PrID *int   `json:"pr_id"`
		} `json:"data"`
	}
	q := url.Values{"limit": {"100"}}
	if err := c.do(ctx, http.MethodGet, "/applications/"+appUUID+"/deployments", q, nil, &page); err != nil {
		return "", err
	}
	for _, d := range page.Data {
		if d.PrID != nil && *d.PrID == pr {
			return d.Uuid, nil
		}
	}
	return "", fmt.Errorf("no deployment yet for the preview of PR #%d", pr)
}

func printLog(l logLine) {
	if l.Channel == "system" {
		fmt.Fprintf(os.Stderr, "· %s\n", l.Message)
		return
	}
	fmt.Println(l.Message)
}

// streamSSE consumes a text/event-stream, decoding each `data:` line as a
// logLine and invoking fn. Reconnection/Last-Event-ID resume is left to a
// future iteration (v1 streams until the command is interrupted).
func (c *Client) streamSSE(ctx context.Context, path string, query url.Values, fn func(logLine)) error {
	u := c.base + "/api/v1" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := (&http.Client{}).Do(req) // no timeout: long-lived stream
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return decodeError(resp)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		var l logLine
		if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &l); err == nil {
			fn(l)
		}
	}
	return sc.Err()
}

// streamDeploymentLogs streams one deployment's logs (SSE); empty uuid = the
// latest deployment of the application.
func (c *Client) streamDeploymentLogs(ctx context.Context, appUUID, deploymentUUID string) error {
	if deploymentUUID == "" || deploymentUUID == "latest" {
		var page struct {
			Data []struct {
				Uuid string `json:"uuid"`
			} `json:"data"`
		}
		q := url.Values{"limit": {"1"}}
		if err := c.do(ctx, http.MethodGet, "/applications/"+appUUID+"/deployments", q, nil, &page); err != nil {
			return err
		}
		if len(page.Data) == 0 {
			return fmt.Errorf("no deployment yet for this application")
		}
		deploymentUUID = page.Data[0].Uuid
	}
	return c.streamSSE(ctx, "/deployments/"+deploymentUUID+"/logs", nil, printLog)
}
