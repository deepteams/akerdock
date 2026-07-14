package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"unicode"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/httpapi"
)

// slugify derives the internal slug of a named resource (§19.2): lowercase
// ASCII, hyphen-separated. Returns "" when nothing usable remains.
func slugify(name string) string {
	var b strings.Builder
	lastHyphen := true // avoid a leading hyphen
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			lastHyphen = false
		case unicode.IsSpace(r) || r == '-' || r == '_' || r == '.' || r == '/':
			if !lastHyphen {
				b.WriteByte('-')
				lastHyphen = true
			}
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

// etagFor renders the optimistic version as a strong ETag (OpenAPI preamble).
func etagFor(version int32) string { return fmt.Sprintf("%q", strconv.Itoa(int(version))) }

// ifMatchVersion parses an If-Match header against the optimistic version.
// Absent header → the caller falls back to the current version (If-Match is
// not required on these PATCHes). A malformed header reads as version 0,
// which can never match.
func ifMatchVersion(r *http.Request, current int32) int32 {
	raw := strings.TrimSpace(r.Header.Get("If-Match"))
	if raw == "" {
		return current
	}
	raw = strings.Trim(raw, `"`)
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return int32(v)
}

// writeVersionConflict emits the 409 version_conflict of the OpenAPI
// preamble, with the current version in details.
func writeVersionConflict(w http.ResponseWriter, r *http.Request, current int32) {
	details := []api.ErrorDetail{{
		Code:    ptr("current_version"),
		Message: "current version is " + strconv.Itoa(int(current)),
	}}
	httpapi.WriteJSON(w, http.StatusConflict, api.Error{
		Code:      "version_conflict",
		Message:   "the resource was modified concurrently — re-read it and retry with the new ETag",
		Details:   &details,
		RequestId: middleware.GetReqID(r.Context()),
	})
}

// patchBody decodes a partial-update body while keeping track of which
// fields were present, so "field absent" and "field: null" stay distinct.
type patchBody struct {
	fields map[string]json.RawMessage
}

func decodePatch(w http.ResponseWriter, r *http.Request, into any) (*patchBody, bool) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "unreadable body")
		return nil, false
	}
	if err := json.Unmarshal(raw, into); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return nil, false
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &fields); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return nil, false
	}
	return &patchBody{fields: fields}, true
}

// Has reports whether the field was present in the body (even as null).
func (p *patchBody) Has(field string) bool {
	_, ok := p.fields[field]
	return ok
}

// IsNull reports whether the field was explicitly null.
func (p *patchBody) IsNull(field string) bool {
	v, ok := p.fields[field]
	return ok && bytes.Equal(bytes.TrimSpace(v), []byte("null"))
}

// isUniqueViolation reports a PostgreSQL 23505 error (used to map slug
// collisions to 409 already_exists).
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
