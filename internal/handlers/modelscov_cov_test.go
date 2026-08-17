package handlers

// Coverage tests for the models handlers (ADR-080): the ADR-079 placement
// guard, the tier-2 flag hygiene, the soft occupied-GPU start guard with its
// swap, the command export/import endpoints and the credentials permission.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
)

func TestModelscovCreateModel(t *testing.T) {
	run := func(t *testing.T, body string, want int, steer func(*rescovDB)) *rescovDB {
		t.Helper()
		a, db := rescovAPI(t)
		if steer != nil {
			steer(db)
		}
		rec := httptest.NewRecorder()
		a.CreateModel(rec, rescovReq(http.MethodPost, "/models", body), api.CreateModelParams{})
		rescovWant(t, rec, want)
		return db
	}
	valid := `{"name":"llm","engine":"vllm","model_id":"org/m","project_uuid":"` + fixtureUUID +
		`","environment_uuid":"` + fixtureUUID + `","server_uuid":"` + fixtureUUID + `","published_port":18001`

	t.Run("invalid json", func(t *testing.T) {
		run(t, "nope", http.StatusBadRequest, nil)
	})
	t.Run("engine and model are validated by name", func(t *testing.T) {
		run(t, `{"name":"llm","engine":"ollama","model_id":"","project_uuid":"`+fixtureUUID+
			`","environment_uuid":"`+fixtureUUID+`","server_uuid":"`+fixtureUUID+`"}`,
			http.StatusUnprocessableEntity, nil)
	})
	t.Run("a reserved tier-2 flag is refused by name", func(t *testing.T) {
		run(t, valid+`,"engine_flags":[{"flag":"--api-key","value":"x"}]}`,
			http.StatusUnprocessableEntity, nil)
	})
	t.Run("a GPU-less server refuses placement (ADR-079)", func(t *testing.T) {
		// nilPtrs leaves every pointer column NULL — gpu_name included.
		run(t, valid+`}`, http.StatusUnprocessableEntity, func(db *rescovDB) {
			db.nilPtrs = true
		})
	})
	t.Run("creation persists the model and its enveloped key", func(t *testing.T) {
		db := run(t, valid+`,"engine_flags":[{"flag":"--enable-prefix-caching"}]}`, http.StatusCreated, nil)
		args := db.lastArgs["CreateModelRow"]
		if len(args) == 0 {
			t.Fatal("the model row never reached the insert")
		}
		var sawKey bool
		for _, arg := range args {
			if b, ok := arg.([]byte); ok && len(b) > 16 {
				sawKey = true // the enveloped api key — ciphertext, never plaintext
			}
		}
		if !sawKey {
			t.Fatalf("no enveloped key in the insert args: %v", args)
		}
	})
}

func TestModelscovLifecycleAndSwap(t *testing.T) {
	t.Run("starting into an occupied GPU answers 409 naming it", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		// The fixture DB answers one running model on the server.
		a.StartModel(rec, rescovReq(http.MethodPost, "/models/"+fixtureUUID+"/start", `{}`), fixtureUUID)
		rescovWant(t, rec, http.StatusConflict)
		if !strings.Contains(rec.Body.String(), "swap=true") {
			t.Fatalf("the refusal must offer the swap: %s", rec.Body.String())
		}
	})
	t.Run("swap=true enqueues one ordered job carrying the neighbour", func(t *testing.T) {
		a, db := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.StartModel(rec, rescovReq(http.MethodPost, "/models/"+fixtureUUID+"/start", `{"swap":true}`), fixtureUUID)
		rescovWant(t, rec, http.StatusAccepted)
		var payload string
		for _, arg := range db.lastArgs["EnqueueJob"] {
			if b, ok := arg.([]byte); ok && strings.Contains(string(b), "stop_resource_id") {
				payload = string(b)
			}
			if s, ok := arg.(string); ok && strings.Contains(s, "stop_resource_id") {
				payload = s
			}
		}
		if payload == "" {
			t.Fatalf("the enqueued payload must carry the neighbour to stop first: %v", db.lastArgs["EnqueueJob"])
		}
	})
	t.Run("a free GPU starts without ceremony", func(t *testing.T) {
		a, db := rescovAPI(t)
		db.noRowsOn["ListRunningModelsOnServer"] = true
		rec := httptest.NewRecorder()
		a.StartModel(rec, rescovReq(http.MethodPost, "/models/"+fixtureUUID+"/start", ``), fixtureUUID)
		rescovWant(t, rec, http.StatusAccepted)
	})
	t.Run("stop and restart accept", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.StopModel(rec, rescovReq(http.MethodPost, "/models/"+fixtureUUID+"/stop", ``), fixtureUUID)
		rescovWant(t, rec, http.StatusAccepted)
		rec = httptest.NewRecorder()
		a.RestartModel(rec, rescovReq(http.MethodPost, "/models/"+fixtureUUID+"/restart", ``), fixtureUUID)
		rescovWant(t, rec, http.StatusAccepted)
		rec = httptest.NewRecorder()
		a.DeleteModel(rec, rescovReq(http.MethodDelete, "/models/"+fixtureUUID, ``), fixtureUUID)
		rescovWant(t, rec, http.StatusAccepted)
	})
}

func TestModelscovListGetUpdate(t *testing.T) {
	t.Run("list and get answer", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.ListModels(rec, rescovReq(http.MethodGet, "/models", ``), api.ListModelsParams{})
		rescovWant(t, rec, http.StatusOK)
		rec = httptest.NewRecorder()
		a.GetModel(rec, rescovReq(http.MethodGet, "/models/"+fixtureUUID, ``), fixtureUUID)
		rescovWant(t, rec, http.StatusOK)
	})
	t.Run("update refuses reserved flags and accepts a rename", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.UpdateModel(rec, rescovReq(http.MethodPatch, "/models/"+fixtureUUID, `{"engine_flags":[{"flag":"--port","value":"1"}]}`),
			fixtureUUID, api.UpdateModelParams{IfMatch: `"1"`})
		rescovWant(t, rec, http.StatusUnprocessableEntity)
		rec = httptest.NewRecorder()
		a.UpdateModel(rec, rescovReq(http.MethodPatch, "/models/"+fixtureUUID, `{"name":"n2","memory_fraction":0.9}`),
			fixtureUUID, api.UpdateModelParams{IfMatch: `"1"`})
		rescovWant(t, rec, http.StatusOK)
	})
}

func TestModelscovCommandAndCredentials(t *testing.T) {
	t.Run("the exported command is the renderer's, masked by default", func(t *testing.T) {
		a, db := rescovAPI(t)
		db.emptyBytes = true // engine_flags NULL, like a fresh row's default
		rec := httptest.NewRecorder()
		a.GetModelCommand(rec, rescovReq(http.MethodGet, "/models/"+fixtureUUID+"/command", ``),
			fixtureUUID, api.GetModelCommandParams{})
		rescovWant(t, rec, http.StatusOK)
		var out api.ModelCommand
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if !out.Masked || !strings.Contains(out.Command, "'****'") || !strings.Contains(out.Command, "vllm serve") {
			t.Fatalf("command = %+v", out)
		}
	})
	t.Run("revealing needs models:credentials", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		reveal := true
		a.GetModelCommand(rec, rescovReq(http.MethodGet, "/models/"+fixtureUUID+"/command", ``,
			string(auth.PermModelsRead)), fixtureUUID, api.GetModelCommandParams{Reveal: &reveal})
		rescovWant(t, rec, http.StatusForbidden)
	})
	t.Run("credentials require their permission", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.GetModelCredentials(rec, rescovReq(http.MethodGet, "/models/"+fixtureUUID+"/credentials", ``,
			string(auth.PermModelsRead)), fixtureUUID)
		rescovWant(t, rec, http.StatusForbidden)
	})
}

func TestModelscovServerHFSurface(t *testing.T) {
	t.Run("the token is stored enveloped, cleared explicitly, never echoed", func(t *testing.T) {
		a, db := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.SetServerHFToken(rec, rescovReq(http.MethodPut, "/servers/"+fixtureUUID+"/hf-token",
			`{"token":"hf_secret_value"}`), fixtureUUID)
		rescovWant(t, rec, http.StatusNoContent)
		if rec.Body.Len() != 0 {
			t.Fatalf("the token endpoint must echo nothing: %s", rec.Body.String())
		}
		for _, arg := range db.lastArgs["SetServerHFToken"] {
			if b, ok := arg.([]byte); ok && strings.Contains(string(b), "hf_secret_value") {
				t.Fatal("the token reached the store in plaintext")
			}
		}
		rec = httptest.NewRecorder()
		a.SetServerHFToken(rec, rescovReq(http.MethodPut, "/servers/"+fixtureUUID+"/hf-token",
			`{"token":""}`), fixtureUUID)
		rescovWant(t, rec, http.StatusNoContent)
	})
	t.Run("deleting is always explicit — exactly one of model_id or all", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.DeleteServerHFCache(rec, rescovReq(http.MethodDelete, "/servers/"+fixtureUUID+"/hf-cache", ``),
			fixtureUUID, api.DeleteServerHFCacheParams{})
		rescovWant(t, rec, http.StatusBadRequest)
		rec = httptest.NewRecorder()
		both := true
		model := "org/name"
		a.DeleteServerHFCache(rec, rescovReq(http.MethodDelete, "/servers/"+fixtureUUID+"/hf-cache", ``),
			fixtureUUID, api.DeleteServerHFCacheParams{ModelId: &model, All: &both})
		rescovWant(t, rec, http.StatusBadRequest)
	})
	t.Run("cache operations need the agent channel", func(t *testing.T) {
		// rescov wires no AgentRPC: the nil-safe resolution answers 409 —
		// exactly what a disconnected agent answers in production.
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.ListServerHFCache(rec, rescovReq(http.MethodGet, "/servers/"+fixtureUUID+"/hf-cache", ``), fixtureUUID)
		rescovWant(t, rec, http.StatusConflict)
		rec = httptest.NewRecorder()
		model := "org/name"
		a.DeleteServerHFCache(rec, rescovReq(http.MethodDelete, "/servers/"+fixtureUUID+"/hf-cache", ``),
			fixtureUUID, api.DeleteServerHFCacheParams{ModelId: &model})
		rescovWant(t, rec, http.StatusConflict)
	})
}

func TestModelscovParseAndSearch(t *testing.T) {
	t.Run("a pasted command parses into the two tiers", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.ParseModelCommand(rec, rescovReq(http.MethodPost, "/models/parse-command",
			`{"command":"vllm serve org/m --max-model-len 4096 --enable-prefix-caching --port 9999"}`))
		rescovWant(t, rec, http.StatusOK)
		var out api.ModelParseResult
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.ModelId != "org/m" || out.MaxModelLen == nil || *out.MaxModelLen != 4096 ||
			len(out.EngineFlags) != 1 || len(out.Notices) != 1 {
			t.Fatalf("parse = %+v", out)
		}
	})
	t.Run("the form preview is the renderer's, always masked", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.PreviewModelCommand(rec, rescovReq(http.MethodPost, "/models/preview-command",
			`{"engine":"sglang","model_id":"org/m","memory_fraction":0.7,"engine_flags":[{"flag":"--enable-torch-compile"}]}`))
		rescovWant(t, rec, http.StatusOK)
		body := rec.Body.String()
		if !strings.Contains(body, "sglang.launch_server") || !strings.Contains(body, "--mem-fraction-static 0.7") {
			t.Fatalf("preview = %s", body)
		}
		rec = httptest.NewRecorder()
		a.PreviewModelCommand(rec, rescovReq(http.MethodPost, "/models/preview-command",
			`{"engine":"vllm","model_id":"x","engine_flags":[{"flag":"--port","value":"1"}]}`))
		rescovWant(t, rec, http.StatusUnprocessableEntity)
	})
	t.Run("an unparseable command is a 422 naming the cause", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.ParseModelCommand(rec, rescovReq(http.MethodPost, "/models/parse-command", `{"command":"vllm serve"}`))
		rescovWant(t, rec, http.StatusUnprocessableEntity)
	})
	t.Run("the hub search proxies and degrades offline", func(t *testing.T) {
		a, _ := rescovAPI(t)
		a.hubSearch = func(_ context.Context, q string) ([]byte, error) {
			return []byte(`[{"id":"org/m","downloads":42,"gated":"manual"}]`), nil
		}
		rec := httptest.NewRecorder()
		a.SearchModelHub(rec, rescovReq(http.MethodGet, "/models/search?q=llama", ``), api.SearchModelHubParams{Q: ptr("llama")})
		rescovWant(t, rec, http.StatusOK)
		if body := rec.Body.String(); !strings.Contains(body, `"org/m"`) || !strings.Contains(body, `"gated":true`) {
			t.Fatalf("search body = %s", body)
		}

		a.hubSearch = func(context.Context, string) ([]byte, error) { return nil, context.DeadlineExceeded }
		rec = httptest.NewRecorder()
		a.SearchModelHub(rec, rescovReq(http.MethodGet, "/models/search?q=llama", ``), api.SearchModelHubParams{Q: ptr("llama")})
		rescovWant(t, rec, http.StatusOK)
		if body := rec.Body.String(); !strings.Contains(body, `"data":[]`) {
			t.Fatalf("offline must degrade to an empty page: %s", body)
		}
	})
	t.Run("a one-letter query is refused", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.SearchModelHub(rec, rescovReq(http.MethodGet, "/models/search?q=l", ``), api.SearchModelHubParams{Q: ptr("l")})
		rescovWant(t, rec, http.StatusBadRequest)
	})
}
