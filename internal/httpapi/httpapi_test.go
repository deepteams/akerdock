package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/go-chi/chi/v5/middleware"
)

func TestJSONErrorHelpers(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request = request.WithContext(context.WithValue(request.Context(), middleware.RequestIDKey, "req-42"))

	t.Run("simple error", func(t *testing.T) {
		rec := httptest.NewRecorder()
		WriteError(rec, request, http.StatusNotFound, CodeNotFound, "missing")
		var got api.Error
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusNotFound || got.Code != CodeNotFound || got.RequestId != "req-42" ||
			rec.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("response = %d %+v %v", rec.Code, got, rec.Header())
		}
	})

	field := "name"
	details := []api.ErrorDetail{{Field: &field, Message: "required"}}
	for _, test := range []struct {
		name   string
		write  func(http.ResponseWriter)
		status int
		code   string
	}{
		{
			name:   "validation",
			write:  func(w http.ResponseWriter) { WriteValidationError(w, request, details) },
			status: http.StatusUnprocessableEntity,
			code:   CodeValidationFailed,
		},
		{
			name: "details",
			write: func(w http.ResponseWriter) {
				WriteErrorDetails(w, request, http.StatusConflict, CodeConflict, "blocked", details)
			},
			status: http.StatusConflict,
			code:   CodeConflict,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			test.write(rec)
			var got api.Error
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if rec.Code != test.status || got.Code != test.code || got.Details == nil || len(*got.Details) != 1 {
				t.Fatalf("response = %d %+v", rec.Code, got)
			}
		})
	}
}

type flushRecorder struct {
	*httptest.ResponseRecorder
	flushed bool
}

func (f *flushRecorder) Flush() { f.flushed = true }

func TestCaptureHonorsHTTPWriterSemantics(t *testing.T) {
	base := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	captured := &capture{ResponseWriter: base}
	captured.WriteHeader(http.StatusCreated)
	captured.WriteHeader(http.StatusTeapot)
	if _, err := captured.Write([]byte(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	captured.Flush()
	if captured.status != http.StatusCreated || base.Code != http.StatusCreated ||
		captured.body.String() != `{"ok":true}` || !base.flushed {
		t.Fatalf("capture = status %d body %q flushed %v", captured.status, captured.body.String(), base.flushed)
	}

	withoutFlusher := &capture{ResponseWriter: httptest.NewRecorder()}
	withoutFlusher.Flush() // must be a safe no-op
}

type errorReadCloser struct{}

func (errorReadCloser) Read([]byte) (int, error) { return 0, errors.New("read failed") }
func (errorReadCloser) Close() error             { return nil }
