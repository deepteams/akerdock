package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

type logLine struct {
	Sequence  int    `json:"sequence"`
	Timestamp string `json:"timestamp"`
	Channel   string `json:"channel"`
	Message   string `json:"message"`
}

func logsCmd() *cobra.Command {
	var (
		component  string
		lines      int
		follow     bool
		deployment string
		deployFlag bool
	)
	cmd := &cobra.Command{
		Use:     "logs REF",
		Short:   "Show container logs (snapshot or -f), or a deployment's logs",
		Example: "  akerdock logs app/varuna\n  akerdock logs app/varuna -f -c postgres",
		Args:    usageArgs(1, "logs <type/name>", "logs app/varuna"),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := newClient(flags.context)
			if err != nil {
				return err
			}
			r, err := parseRef(args[0])
			if err != nil {
				return err
			}
			if r.kind != "apps" {
				return fmt.Errorf("logs currently support app/… references")
			}
			res, err := c.resolve(cmd.Context(), r)
			if err != nil {
				return err
			}

			// Deployment logs: SSE stream (build), resumable.
			if deployFlag {
				return c.streamDeploymentLogs(cmd.Context(), res.Uuid, deployment)
			}

			q := url.Values{}
			if component != "" {
				q.Set("component", component)
			}
			if follow {
				return c.streamSSE(cmd.Context(), "/applications/"+res.Uuid+"/logs/stream", q, printLog)
			}
			q.Set("lines", strconv.Itoa(lines))
			var page struct {
				Data []logLine `json:"data"`
			}
			if err := c.do(cmd.Context(), http.MethodGet, "/applications/"+res.Uuid+"/logs", q, nil, &page); err != nil {
				return err
			}
			if flags.output == "json" {
				return printJSON(page.Data)
			}
			for _, l := range page.Data {
				printLog(l)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVarP(&component, "component", "c", "", "compose service to read logs from")
	f.IntVarP(&lines, "lines", "n", 200, "number of lines (snapshot)")
	f.BoolVarP(&follow, "follow", "f", false, "stream logs as they arrive")
	f.StringVar(&deployment, "deployment", "", "read a deployment's logs (empty = latest)")
	cmd.Flags().Lookup("deployment").NoOptDefVal = "latest"
	cmd.PreRun = func(cmd *cobra.Command, _ []string) {
		deployFlag = cmd.Flags().Changed("deployment")
	}
	return cmd
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
