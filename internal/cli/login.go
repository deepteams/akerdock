package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func loginCmd() *cobra.Command {
	var (
		urlFlag   string
		ctxName   string
		scopes    string
		withToken bool
		noBrowser bool
	)
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Authenticate to an AkerDock instance",
		Long: "Authenticate through the browser (SSO/password/passkey) without opening any\n" +
			"local port, or paste an existing API token with --with-token.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if urlFlag == "" {
				return fmt.Errorf("--url is required (e.g. --url https://manager.example.com)")
			}
			base := strings.TrimRight(urlFlag, "/")
			u, err := url.Parse(base)
			if err != nil || u.Host == "" {
				return fmt.Errorf("invalid --url %q", urlFlag)
			}
			name := ctxName
			if name == "" {
				name = u.Host
			}

			var token, team string
			if withToken {
				token, team, err = loginWithToken(cmd.Context(), base)
			} else {
				token, team, err = loginBrowser(cmd.Context(), base, scopes, noBrowser)
			}
			if err != nil {
				return err
			}

			cfg, err := LoadConfig()
			if err != nil {
				return err
			}
			cfg.Contexts[name] = Context{URL: base, Fqdn: u.Host, TeamUUID: team}
			cfg.CurrentContext = name
			if err := cfg.Save(); err != nil {
				return err
			}
			if err := setToken(name, token); err != nil {
				return err
			}
			fmt.Printf("logged in to %s as context %q\n", base, name)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&urlFlag, "url", "", "instance base URL (https://…)")
	f.StringVar(&ctxName, "context", "", "context name to create/update (default: host)")
	f.StringVar(&scopes, "scopes", "read,write", "requested permissions")
	f.BoolVar(&withToken, "with-token", false, "read an existing API token from stdin instead of using the browser")
	f.BoolVar(&noBrowser, "no-browser", false, "print the URL instead of opening a browser")
	return cmd
}

// loginWithToken reads a token from stdin, verifies it, and resolves its team.
func loginWithToken(ctx context.Context, base string) (token, team string, err error) {
	fmt.Fprint(os.Stderr, "Paste an AkerDock API token: ")
	if term.IsTerminal(int(os.Stdin.Fd())) {
		raw, rerr := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if rerr != nil {
			return "", "", rerr
		}
		token = strings.TrimSpace(string(raw))
	} else {
		sc := bufio.NewScanner(os.Stdin)
		sc.Scan()
		token = strings.TrimSpace(sc.Text())
	}
	if token == "" {
		return "", "", fmt.Errorf("no token provided")
	}
	c := &Client{base: strings.TrimRight(base, "/"), token: token, http: &http.Client{Timeout: 30 * time.Second}}
	var page struct {
		Data []struct {
			Uuid string `json:"uuid"`
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/teams", nil, nil, &page); err != nil {
		return "", "", fmt.Errorf("token rejected: %w", err)
	}
	switch len(page.Data) {
	case 0:
		return token, "", nil
	case 1:
		return token, page.Data[0].Uuid, nil
	default:
		fmt.Fprintln(os.Stderr, "Token has access to several teams; pick one with `akerdock context use` later.")
		return token, page.Data[0].Uuid, nil
	}
}

// loginBrowser runs the poll+code+PKCE flow (ADR-031): no local port, only
// outbound HTTPS to the manager.
func loginBrowser(ctx context.Context, base, scopes string, noBrowser bool) (token, team string, err error) {
	verifier, challenge, err := pkcePair()
	if err != nil {
		return "", "", err
	}
	host, _ := os.Hostname()
	user := os.Getenv("USER")
	if user == "" {
		user = os.Getenv("USERNAME")
	}
	name := fmt.Sprintf("%s@%s", user, host)

	// 1. start
	var start struct {
		RequestID string `json:"request_id"`
		UserCode  string `json:"user_code"`
		VerifyURL string `json:"verify_url"`
		Interval  int    `json:"interval"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := postJSON(ctx, base+"/auth/cli/start",
		map[string]any{"challenge": challenge, "name": name, "scopes": scopes}, &start); err != nil {
		return "", "", fmt.Errorf("login start failed: %w", err)
	}

	verifyURL := start.VerifyURL
	if verifyURL == "" {
		verifyURL = fmt.Sprintf("%s/cli/authorize?request_id=%s", strings.TrimRight(base, "/"), url.QueryEscape(start.RequestID))
	}
	fmt.Fprintf(os.Stderr, "\n  Confirmation code: %s\n", start.UserCode)
	fmt.Fprintf(os.Stderr, "  Open: %s\n\n", verifyURL)
	if !noBrowser {
		_ = openBrowser(verifyURL)
	}
	fmt.Fprintln(os.Stderr, "Waiting for approval in the browser…")

	// 2. poll token
	interval := time.Duration(max(start.Interval, 2)) * time.Second
	deadline := time.Now().Add(time.Duration(max(start.ExpiresIn, 600)) * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(interval):
		}
		var resp struct {
			Status   string `json:"status"`
			Token    string `json:"token"`
			TeamUUID string `json:"team_uuid"`
		}
		err := postJSON(ctx, base+"/auth/cli/token",
			map[string]any{"request_id": start.RequestID, "verifier": verifier}, &resp)
		if err != nil {
			return "", "", err
		}
		if resp.Status == "pending" || resp.Token == "" {
			continue
		}
		return resp.Token, resp.TeamUUID, nil
	}
	return "", "", fmt.Errorf("login timed out — re-run `akerdock login`")
}

// pkcePair returns a random verifier and its base64url SHA-256 challenge.
func pkcePair() (verifier, challenge string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// postJSON posts a JSON body to an absolute URL (used for the out-of-contract
// /auth/cli/* endpoints, which live outside /api/v1).
func postJSON(ctx context.Context, absURL string, body, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, absURL, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return decodeError(resp)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// openBrowser best-effort opens a URL in the default browser.
func openBrowser(u string) error {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "windows":
		cmd, args = "rundll32", []string{"url.dll,FileProtocolHandler"}
	default:
		cmd = "xdg-open"
	}
	return exec.Command(cmd, append(args, u)...).Start()
}
