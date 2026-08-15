package handlers

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/httpapi"
)

const (
	defaultLimit = 25
	maxLimit     = 100
)

// pageLimit validates the ?limit= parameter (1–100, default 25).
func pageLimit(w http.ResponseWriter, r *http.Request, limit *int) (int32, bool) {
	if limit == nil {
		return defaultLimit, true
	}
	if *limit < 1 || *limit > maxLimit {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "limit must be between 1 and 100")
		return 0, false
	}
	return int32(*limit), true
}

// uuidFilter parses an optional UUID query filter (?project_uuid=…). Absent or
// empty means "no filter" (NULL UUID); a malformed value is a validation error
// — ignoring it would silently return the unfiltered, team-wide list, which is
// exactly the leak the filter exists to prevent.
func uuidFilter(w http.ResponseWriter, r *http.Request, raw *string, field string) (pgtype.UUID, bool) {
	var u pgtype.UUID
	if raw == nil || *raw == "" {
		return u, true
	}
	if err := u.Scan(*raw); err != nil {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
			Field: ptr(field), Code: ptr("invalid"), Message: field + " must be a UUID",
		}})
		return u, false
	}
	return u, true
}

// Cursors are opaque (OpenAPI preamble): base64url of "v1:<last internal id>".
func encodeCursor(lastID int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte("v1:" + strconv.FormatInt(lastID, 10)))
}

// afterID decodes the ?cursor= parameter; 0 means "first page".
func afterID(w http.ResponseWriter, r *http.Request, cursor *string) (int64, bool) {
	if cursor == nil || *cursor == "" {
		return 0, true
	}
	invalid := func() (int64, bool) {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid cursor")
		return 0, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(*cursor)
	if err != nil {
		return invalid()
	}
	payload, ok := strings.CutPrefix(string(raw), "v1:")
	if !ok {
		return invalid()
	}
	id, err := strconv.ParseInt(payload, 10, 64)
	if err != nil || id <= 0 {
		return invalid()
	}
	return id, true
}

// nextCursor computes the next_cursor of a page fetched with limit+1 rows:
// nil on the last page.
func nextCursor[T any](rows []T, limit int32, lastID func(T) int64) ([]T, *string) {
	if len(rows) <= int(limit) {
		return rows, nil
	}
	rows = rows[:limit]
	c := encodeCursor(lastID(rows[len(rows)-1]))
	return rows, &c
}

func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", u.Bytes[0:4], u.Bytes[4:6], u.Bytes[6:8], u.Bytes[8:10], u.Bytes[10:16])
}

// optionalUUID renders a nullable UUID column: absent rather than an empty
// string when the row carries no value.
func optionalUUID(u pgtype.UUID) *string {
	if !u.Valid {
		return nil
	}
	return ptr(uuidString(u))
}

func timePtr(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time.UTC()
	return &t
}

func ptr[T any](v T) *T { return &v }
