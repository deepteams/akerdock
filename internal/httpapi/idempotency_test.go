package httpapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/store"
)

type fakeIdempotencyStore struct {
	claim       func(store.ClaimIdempotencyKeyParams) (store.ClaimIdempotencyKeyRow, error)
	completed   []store.CompleteIdempotencyKeyParams
	completeErr error
}

func (f *fakeIdempotencyStore) ClaimIdempotencyKey(_ context.Context, params store.ClaimIdempotencyKeyParams) (store.ClaimIdempotencyKeyRow, error) {
	return f.claim(params)
}

func (f *fakeIdempotencyStore) CompleteIdempotencyKey(_ context.Context, params store.CompleteIdempotencyKeyParams) error {
	f.completed = append(f.completed, params)
	return f.completeErr
}

func idempotentRequest(body string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/apps?force=true", strings.NewReader(body))
	request.Header.Set("Idempotency-Key", "key-1")
	return request
}

func runIdempotency(t *testing.T, storeFake *fakeIdempotencyStore, request *http.Request, teamOK bool, next http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	middleware := (&Idempotency{Store: storeFake}).Handler(func(*http.Request) (int64, bool) {
		return 42, teamOK
	})
	middleware(next).ServeHTTP(rec, request)
	return rec
}

func TestIdempotencyPassThrough(t *testing.T) {
	called := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called++
		w.WriteHeader(http.StatusNoContent)
	})
	storeFake := &fakeIdempotencyStore{claim: func(store.ClaimIdempotencyKeyParams) (store.ClaimIdempotencyKeyRow, error) {
		t.Fatal("store should not be called")
		return store.ClaimIdempotencyKeyRow{}, nil
	}}

	request := httptest.NewRequest(http.MethodGet, "/apps", nil)
	runIdempotency(t, storeFake, request, true, next)
	runIdempotency(t, storeFake, idempotentRequest(`{}`), false, next)
	if called != 2 {
		t.Fatalf("next called %d times", called)
	}
}

func TestIdempotencyRejectsUnreadableAndOversizedBodies(t *testing.T) {
	storeFake := &fakeIdempotencyStore{claim: func(store.ClaimIdempotencyKeyParams) (store.ClaimIdempotencyKeyRow, error) {
		t.Fatal("store should not be called")
		return store.ClaimIdempotencyKeyRow{}, nil
	}}
	unreadable := idempotentRequest("")
	unreadable.Body = errorReadCloser{}
	if rec := runIdempotency(t, storeFake, unreadable, true, func(http.ResponseWriter, *http.Request) {
		t.Fatal("next should not run")
	}); rec.Code != http.StatusBadRequest {
		t.Fatalf("unreadable body status = %d", rec.Code)
	}

	oversized := idempotentRequest("")
	oversized.Body = io.NopCloser(bytes.NewReader(make([]byte, maxBodyBytes+1)))
	if rec := runIdempotency(t, storeFake, oversized, true, func(http.ResponseWriter, *http.Request) {
		t.Fatal("next should not run")
	}); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body status = %d", rec.Code)
	}
}

func TestIdempotencyClaimFailureAndConflicts(t *testing.T) {
	t.Run("store failure", func(t *testing.T) {
		storeFake := &fakeIdempotencyStore{claim: func(store.ClaimIdempotencyKeyParams) (store.ClaimIdempotencyKeyRow, error) {
			return store.ClaimIdempotencyKeyRow{}, errors.New("database unavailable")
		}}
		rec := runIdempotency(t, storeFake, idempotentRequest(`{}`), true, func(http.ResponseWriter, *http.Request) {
			t.Fatal("next should not run")
		})
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d", rec.Code)
		}
	})

	t.Run("different request", func(t *testing.T) {
		storeFake := &fakeIdempotencyStore{claim: func(store.ClaimIdempotencyKeyParams) (store.ClaimIdempotencyKeyRow, error) {
			return store.ClaimIdempotencyKeyRow{RequestHash: "other", IsNew: false}, nil
		}}
		rec := runIdempotency(t, storeFake, idempotentRequest(`{"name":"a"}`), true, func(http.ResponseWriter, *http.Request) {
			t.Fatal("next should not run")
		})
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "idempotency_conflict") {
			t.Fatalf("response = %d %q", rec.Code, rec.Body.String())
		}
	})

	t.Run("in progress", func(t *testing.T) {
		storeFake := &fakeIdempotencyStore{claim: func(params store.ClaimIdempotencyKeyParams) (store.ClaimIdempotencyKeyRow, error) {
			return store.ClaimIdempotencyKeyRow{RequestHash: params.RequestHash, IsNew: false}, nil
		}}
		rec := runIdempotency(t, storeFake, idempotentRequest(`{}`), true, func(http.ResponseWriter, *http.Request) {
			t.Fatal("next should not run")
		})
		if rec.Code != http.StatusConflict || rec.Header().Get("Retry-After") != "2" {
			t.Fatalf("response = %d headers %v", rec.Code, rec.Header())
		}
	})
}

func TestIdempotencyReplay(t *testing.T) {
	status := int32(http.StatusCreated)
	storeFake := &fakeIdempotencyStore{claim: func(params store.ClaimIdempotencyKeyParams) (store.ClaimIdempotencyKeyRow, error) {
		return store.ClaimIdempotencyKeyRow{
			RequestHash:  params.RequestHash,
			StatusCode:   &status,
			ResponseBody: []byte(`{"uuid":"one"}`),
			CompletedAt:  pgtype.Timestamptz{Valid: true},
			IsNew:        false,
		}, nil
	}}
	rec := runIdempotency(t, storeFake, idempotentRequest(`{}`), true, func(http.ResponseWriter, *http.Request) {
		t.Fatal("next should not run")
	})
	if rec.Code != http.StatusCreated || rec.Header().Get("Idempotency-Replayed") != "true" ||
		rec.Body.String() != `{"uuid":"one"}` {
		t.Fatalf("replay = %d %v %q", rec.Code, rec.Header(), rec.Body.String())
	}
}

func TestIdempotencyCompletesOnlySuccessfulResponses(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status       int
		body         string
		wantBody     string
		wantComplete bool
	}{
		{name: "json success", status: http.StatusCreated, body: `{"ok":true}`, wantBody: `{"ok":true}`, wantComplete: true},
		{name: "non-json success", status: http.StatusOK, body: `plain`, wantBody: `{}`, wantComplete: true},
		{name: "failure", status: http.StatusBadRequest, body: `{"error":true}`, wantComplete: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storeFake := &fakeIdempotencyStore{
				claim: func(params store.ClaimIdempotencyKeyParams) (store.ClaimIdempotencyKeyRow, error) {
					if params.TeamID != 42 || params.Key != "key-1" || params.Endpoint != "POST /apps" || params.RequestHash == "" {
						t.Fatalf("claim params = %+v", params)
					}
					return store.ClaimIdempotencyKeyRow{ID: 7, IsNew: true}, nil
				},
				completeErr: errors.New("ignored best-effort completion failure"),
			}
			rec := runIdempotency(t, storeFake, idempotentRequest(`{"x":1}`), true, func(w http.ResponseWriter, r *http.Request) {
				body := new(bytes.Buffer)
				_, _ = body.ReadFrom(r.Body)
				if body.String() != `{"x":1}` {
					t.Fatalf("next saw body %q", body.String())
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			if rec.Code != tc.status {
				t.Fatalf("status = %d", rec.Code)
			}
			if tc.wantComplete {
				if len(storeFake.completed) != 1 || string(storeFake.completed[0].ResponseBody) != tc.wantBody {
					t.Fatalf("completion = %+v", storeFake.completed)
				}
			} else if len(storeFake.completed) != 0 {
				t.Fatalf("failed response was completed: %+v", storeFake.completed)
			}
		})
	}
}
