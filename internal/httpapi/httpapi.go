// Package httpapi holds the shared HTTP plumbing of the public API: the
// single Error schema of §24.1 and JSON response helpers.
package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/deepteams/akerdock/internal/api"
)

// Stable machine-readable error codes (OpenAPI Error schema).
const (
	CodeBadRequest       = "bad_request"
	CodeUnauthorized     = "unauthorized"
	CodeForbidden        = "forbidden"
	CodeNotFound         = "not_found"
	CodeConflict         = "already_exists"
	CodeValidationFailed = "validation_failed"
	CodeInternal         = "internal"
)

// WriteError emits the Error schema with the request correlation id. It
// never carries secrets or stack traces (§24.1).
func WriteError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	WriteJSON(w, status, api.Error{
		Code:      code,
		Message:   message,
		RequestId: middleware.GetReqID(r.Context()),
	})
}

// WriteValidationError emits a 422 with structured field details.
func WriteValidationError(w http.ResponseWriter, r *http.Request, details []api.ErrorDetail) {
	WriteJSON(w, http.StatusUnprocessableEntity, api.Error{
		Code:      CodeValidationFailed,
		Message:   "validation failed",
		Details:   &details,
		RequestId: middleware.GetReqID(r.Context()),
	})
}

// WriteErrorDetails emits an error that carries structured details — used when
// the caller needs to see WHAT is wrong, not merely that something is (the
// remnants of a failed deletion, §20.6.4).
func WriteErrorDetails(w http.ResponseWriter, r *http.Request, status int, code, message string, details []api.ErrorDetail) {
	WriteJSON(w, status, api.Error{
		Code:      code,
		Message:   message,
		Details:   &details,
		RequestId: middleware.GetReqID(r.Context()),
	})
}

// WriteJSON writes v as a JSON response body with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
