// `akerdock mcp` (ADR-043): speaks MCP over stdio and forwards each JSON-RPC
// message to the instance's /mcp endpoint. A local assistant (desktop app,
// IDE) configures one command instead of an OAuth flow: the credential is the
// CLI context it already has in ~/.akerdock/, or an explicit token.
//
// The CLI implements no protocol logic — it is a transport adapter. Tools,
// schemas and authorization all live on the server, so a client using stdio
// and one using HTTP see exactly the same MCP server.
package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func mcpCmd() *cobra.Command {
	var tokenFlag, urlFlag string
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Run the MCP server over stdio (for a local assistant)",
		Long: "Bridges MCP stdio to this instance's /mcp endpoint (ADR-043).\n\n" +
			"Credentials come from the current CLI context (`akerdock login`), or from\n" +
			"--token/AKERDOCK_TOKEN with --url/AKERDOCK_URL. The tools are read-only.\n\n" +
			"Configure a local assistant with:  akerdock mcp",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			base, token, err := mcpTarget(urlFlag, tokenFlag)
			if err != nil {
				return err
			}
			return mcpBridge(cmd.InOrStdin(), cmd.OutOrStdout(), base, token)
		},
	}
	cmd.Flags().StringVar(&tokenFlag, "token", "", "API token (default: the current context's, or $AKERDOCK_TOKEN)")
	cmd.Flags().StringVar(&urlFlag, "url", "", "instance URL (default: the current context's, or $AKERDOCK_URL)")
	return cmd
}

// mcpTarget resolves where to talk and with what: explicit flags first, then
// the environment, then the CLI context — the same precedence as the rest of
// the CLI, so a configured workstation needs no argument at all.
func mcpTarget(urlFlag, tokenFlag string) (string, string, error) {
	base := firstNonEmpty(urlFlag, os.Getenv("AKERDOCK_URL"))
	token := firstNonEmpty(tokenFlag, os.Getenv("AKERDOCK_TOKEN"))
	if base != "" && token != "" {
		return strings.TrimRight(base, "/"), token, nil
	}
	client, err := newClient(flags.context)
	if err != nil {
		if base == "" && token == "" {
			return "", "", fmt.Errorf("%w\n(or pass --url and --token)", err)
		}
		return "", "", err
	}
	return firstNonEmpty(base, client.base), firstNonEmpty(token, client.token), nil
}

// mcpBridge reads newline-delimited JSON-RPC from in, POSTs each message to
// /mcp and writes the response back. A message the server answers with 202
// (a notification) produces no output, as the protocol requires.
func mcpBridge(in io.Reader, out io.Writer, base, token string) error {
	client := &http.Client{Timeout: 60 * time.Second}
	reader := bufio.NewReaderSize(in, 1<<20)
	writer := bufio.NewWriter(out)
	for {
		line, err := reader.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			resp, rpcErr := mcpForward(client, base, token, bytes.TrimSpace(line))
			if rpcErr != nil {
				// A transport failure is reported IN the protocol: an
				// assistant sees an error message instead of a dead pipe.
				resp = mcpTransportError(bytes.TrimSpace(line), rpcErr)
			}
			if len(resp) > 0 {
				if _, werr := writer.Write(append(resp, '\n')); werr != nil {
					return werr
				}
				if ferr := writer.Flush(); ferr != nil {
					return ferr
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func mcpForward(client *http.Client, base, token string, message []byte) ([]byte, error) {
	req, err := http.NewRequest(http.MethodPost, base+"/mcp", bytes.NewReader(message))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusAccepted:
		return nil, nil // notification: nothing to send back
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("this instance does not expose MCP — enable it in Global settings")
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, fmt.Errorf("the token was refused — run `akerdock login` or pass --token")
	case resp.StatusCode >= 400:
		return nil, fmt.Errorf("instance answered %s: %s", resp.Status, strings.TrimSpace(string(body)))
	default:
		return body, nil
	}
}

// mcpTransportError builds a JSON-RPC error carrying the request's id, so the
// client can match it to its call instead of hanging.
func mcpTransportError(request []byte, cause error) []byte {
	var envelope struct {
		ID json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal(request, &envelope)
	if len(envelope.ID) == 0 {
		return nil // a notification failing has nobody to tell
	}
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      envelope.ID,
		"error":   map[string]any{"code": -32000, "message": cause.Error()},
	})
	if err != nil {
		return nil
	}
	return body
}
