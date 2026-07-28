package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is a thin authenticated HTTP client for one context. It only ever
// targets the context URL (the manager), over its scheme/port — no other
// destination (spec §2).
type Client struct {
	base  string
	token string
	team  string
	http  *http.Client
}

// apiError is the platform's JSON error envelope (httpapi.WriteError).
type apiError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
	// RequestURL is set by 403 access_request_required (ADR-045): the page
	// where the missing access grant is obtained. Being told "no" without
	// being told where to go is how a control turns into an obstacle.
	RequestURL string `json:"request_url"`
	status     int
}

func (e *apiError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("%s (%s)", e.Message, e.Code)
	}
	return fmt.Sprintf("HTTP %d", e.status)
}

// newClient resolves the active context and returns a ready client. It fails
// with an actionable message when no context or token is configured.
func newClient(contextFlag string) (*Client, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return nil, err
	}
	dir, err := loadDirConfig()
	if err != nil {
		return nil, err
	}
	// Resolve context and team through the full precedence chain (spec §4):
	// the explicit contextFlag first, then env, then .akerdock, then global.
	f := flags
	f.context = contextFlag
	s := resolveSettings(f, cfg, dir)
	if s.ContextName == "" {
		return nil, fmt.Errorf("no context selected — run `akerdock login`, or set `context:` in a .akerdock file")
	}
	ctx, ok := cfg.Contexts[s.ContextName]
	if !ok {
		return nil, fmt.Errorf("unknown context %q — see `akerdock context list`", s.ContextName)
	}
	creds, err := loadCredentials()
	if err != nil {
		return nil, err
	}
	token := creds.Tokens[s.ContextName]
	if token == "" {
		return nil, fmt.Errorf("context %q has no token — run `akerdock login --context %s`", s.ContextName, s.ContextName)
	}
	return &Client{
		base:  strings.TrimRight(ctx.URL, "/"),
		token: token,
		team:  s.Team,
		http:  &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// do performs a request against /api/v1, decoding a JSON response into out
// (nil to ignore the body). query is appended verbatim.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	u := c.base + "/api/v1" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach %s: %w", c.base, err)
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

func decodeError(resp *http.Response) error {
	var e apiError
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	_ = json.Unmarshal(data, &e)
	e.status = resp.StatusCode
	if e.Message == "" {
		e.Message = strings.TrimSpace(string(data))
	}
	return &e
}
