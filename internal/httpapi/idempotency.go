package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"

	"github.com/deepteams/akerdock/internal/store"
)

// maxBodyBytes bounds the request body buffered for idempotency hashing.
const maxBodyBytes = 1 << 20

// Idempotency implements the Idempotency-Key contract of §24.1: replaying
// the same key with the same body returns the original response; the same
// key with a different body is a 409 idempotency_conflict.
type Idempotency struct {
	Store *store.Queries
}

// capture buffers the response so a successful one can be replayed later.
type capture struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (c *capture) WriteHeader(status int) {
	c.status = status
	c.ResponseWriter.WriteHeader(status)
}

func (c *capture) Write(p []byte) (int, error) {
	c.body.Write(p)
	return c.ResponseWriter.Write(p)
}

// Flush lets SSE handlers keep streaming through the wrapper.
func (c *capture) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Handler wraps the API routes. Only mutating requests carrying an
// Idempotency-Key are intercepted; everything else passes straight through.
func (i *Idempotency) Handler(teamID func(*http.Request) (int64, bool)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get("Idempotency-Key")
			if key == "" || (r.Method != http.MethodPost && r.Method != http.MethodPut && r.Method != http.MethodPatch) {
				next.ServeHTTP(w, r)
				return
			}
			team, ok := teamID(r)
			if !ok {
				next.ServeHTTP(w, r)
				return
			}

			body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
			if err != nil {
				WriteError(w, r, http.StatusBadRequest, CodeBadRequest, "unreadable body")
				return
			}
			_ = r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(body))

			sum := sha256.Sum256(append([]byte(r.URL.RawQuery+"\n"), body...))
			hash := hex.EncodeToString(sum[:])
			endpoint := r.Method + " " + r.URL.Path

			row, err := i.Store.ClaimIdempotencyKey(r.Context(), store.ClaimIdempotencyKeyParams{
				TeamID: team, Key: key, Endpoint: endpoint, RequestHash: hash,
			})
			if err != nil {
				WriteError(w, r, http.StatusInternalServerError, CodeInternal, "internal error")
				return
			}

			if !row.IsNew {
				if row.RequestHash != hash {
					WriteError(w, r, http.StatusConflict, "idempotency_conflict",
						"this Idempotency-Key was already used with a different request body (§24.1)")
					return
				}
				if row.CompletedAt.Valid && row.StatusCode != nil {
					// Replay the original response verbatim.
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Idempotency-Replayed", "true")
					w.WriteHeader(int(*row.StatusCode))
					_, _ = w.Write(row.ResponseBody)
					return
				}
				// The original request is still in flight: the client must
				// retry rather than run the operation twice.
				w.Header().Set("Retry-After", "2")
				WriteError(w, r, http.StatusConflict, "operation_in_progress",
					"a request with this Idempotency-Key is still in progress — retry shortly")
				return
			}

			rec := &capture{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)

			// Only successful responses are replayable: a failure may be
			// retried with the same key.
			if rec.status >= 200 && rec.status < 300 {
				i.complete(r.Context(), row.ID, rec.status, rec.body.Bytes())
			}
		})
	}
}

func (i *Idempotency) complete(ctx context.Context, id int64, status int, body []byte) {
	if !json.Valid(body) {
		body = []byte("{}")
	}
	code := int32(status)
	_ = i.Store.CompleteIdempotencyKey(ctx, store.CompleteIdempotencyKeyParams{
		ID: id, StatusCode: &code, ResponseBody: body,
	})
}
