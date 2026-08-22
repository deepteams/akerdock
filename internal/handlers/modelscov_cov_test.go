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

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/store"
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
	t.Run("a GPU whose fractions do not fit answers 409 with the arithmetic", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		// The fixture DB answers one running model, and every fraction scans
		// as 1.0: the whole card each, twice over.
		a.StartModel(rec, rescovReq(http.MethodPost, "/models/"+fixtureUUID+"/start", `{}`), fixtureUUID)
		rescovWant(t, rec, http.StatusConflict)
		body := rec.Body.String()
		for _, want := range []string{"swap=true", "force=true", "claims 100%", "200% claimed", "95%"} {
			if !strings.Contains(body, want) {
				t.Fatalf("the refusal must state the arithmetic and both ways past it (%q missing): %s", want, body)
			}
		}
	})
	// ADR-082 §1: the case ADR-080 §5 named as the reason for softness and
	// then made impossible — two small models sharing one card.
	t.Run("fractions that fit start without ceremony", func(t *testing.T) {
		a, db := rescovAPI(t)
		fraction := 0.4
		db.floatFill = &fraction // 0.4 running + 0.4 candidate = 0.8, within budget
		rec := httptest.NewRecorder()
		a.StartModel(rec, rescovReq(http.MethodPost, "/models/"+fixtureUUID+"/start", `{}`), fixtureUUID)
		rescovWant(t, rec, http.StatusAccepted)
	})
	t.Run("force=true starts alongside without stopping anything", func(t *testing.T) {
		a, db := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.StartModel(rec, rescovReq(http.MethodPost, "/models/"+fixtureUUID+"/start", `{"force":true}`), fixtureUUID)
		rescovWant(t, rec, http.StatusAccepted)
		for _, arg := range db.lastArgs["EnqueueJob"] {
			if b, ok := arg.([]byte); ok && strings.Contains(string(b), "stop_resource_id") {
				t.Fatalf("forcing starts BESIDE the neighbours; it must never stop one: %s", b)
			}
		}
	})
	t.Run("swap and force together are refused before anything is enqueued", func(t *testing.T) {
		a, db := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.StartModel(rec, rescovReq(http.MethodPost, "/models/"+fixtureUUID+"/start",
			`{"swap":true,"force":true}`), fixtureUUID)
		rescovWant(t, rec, http.StatusConflict)
		if _, enqueued := db.lastArgs["EnqueueJob"]; enqueued {
			t.Fatal("a contradictory request must not reach the queue")
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

// The follow-up UX tranche: one operation at a time, cancellable while
// queued, variables and logs on the model.
func TestModelscovOperationGuardAndFollowUp(t *testing.T) {
	t.Run("a second start is refused while one is queued", func(t *testing.T) {
		a, db := rescovAPI(t)
		db.countOne = true                              // CountActiveJobsByLockKey answers 1
		db.noRowsOn["ListRunningModelsOnServer"] = true // a free GPU: the guard is what refuses
		rec := httptest.NewRecorder()
		a.StartModel(rec, rescovReq(http.MethodPost, "/models/"+fixtureUUID+"/start", `{}`), fixtureUUID)
		rescovWant(t, rec, http.StatusConflict)
		if !strings.Contains(rec.Body.String(), "already queued or running") {
			t.Fatalf("the refusal must say why: %s", rec.Body.String())
		}
	})
	t.Run("a queued job cancels outright; a running one is asked", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.CancelJob(rec, rescovReq(http.MethodPost, "/jobs/"+fixtureUUID+"/cancel", ``), fixtureUUID)
		rescovWant(t, rec, http.StatusOK)
		if !strings.Contains(rec.Body.String(), "cancelled") {
			t.Fatalf("the cancelled job must come back cancelled: %s", rec.Body.String())
		}
		// Already started: the outright cancel matches nothing, and the
		// cooperative request takes over — 202, because the job stops at its
		// next checkpoint rather than here.
		a2, db2 := rescovAPI(t)
		db2.execTagOn["CancelQueuedJob"] = "UPDATE 0"
		rec = httptest.NewRecorder()
		a2.CancelJob(rec, rescovReq(http.MethodPost, "/jobs/"+fixtureUUID+"/cancel", ``), fixtureUUID)
		rescovWant(t, rec, http.StatusAccepted)
		if !strings.Contains(rec.Body.String(), "cancel_requested_at") {
			t.Fatalf("the acknowledgement must carry the request's timestamp: %s", rec.Body.String())
		}
		// Neither path applies — finished, or a family with no checkpoint at
		// which stopping would be safe.
		a3, db3 := rescovAPI(t)
		db3.execTagOn["CancelQueuedJob"] = "UPDATE 0"
		db3.execTagOn["RequestJobCancel"] = "UPDATE 0"
		rec = httptest.NewRecorder()
		a3.CancelJob(rec, rescovReq(http.MethodPost, "/jobs/"+fixtureUUID+"/cancel", ``), fixtureUUID)
		rescovWant(t, rec, http.StatusConflict)
	})
	t.Run("model variables ride the resource machinery", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.ListModelEnvs(rec, rescovReq(http.MethodGet, "/models/"+fixtureUUID+"/envs", ``),
			fixtureUUID, api.ListModelEnvsParams{})
		rescovWant(t, rec, http.StatusOK)
		rec = httptest.NewRecorder()
		a.CreateModelEnv(rec, rescovReq(http.MethodPost, "/models/"+fixtureUUID+"/envs",
			`{"key":"MY_VAR","value":"x","is_secret":false,"is_build_time":false,"is_literal":false,"is_multiline":false,"is_locked":false}`), fixtureUUID)
		rescovWant(t, rec, http.StatusCreated)
		rec = httptest.NewRecorder()
		a.DeleteModelEnv(rec, rescovReq(http.MethodDelete, "/models/"+fixtureUUID+"/envs/"+fixtureUUID, ``),
			fixtureUUID, fixtureUUID)
		rescovWant(t, rec, http.StatusNoContent)
	})
	t.Run("logs need the agent channel", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.GetModelLogs(rec, rescovReq(http.MethodGet, "/models/"+fixtureUUID+"/logs", ``),
			fixtureUUID, api.GetModelLogsParams{})
		rescovWant(t, rec, http.StatusConflict)
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

// The optional public domain (ADR-080 §1 routed by ADR-077): validation of
// the element forms, the immediate routing regeneration, the instance-wide
// (fqdn, path) uniqueness and the whole-set replacement on update.
func TestModelscovModelDomains(t *testing.T) {
	valid := `{"name":"llm","engine":"vllm","model_id":"org/m","project_uuid":"` + fixtureUUID +
		`","environment_uuid":"` + fixtureUUID + `","server_uuid":"` + fixtureUUID + `","published_port":18001`

	t.Run("a :port element is refused — the engine port is the only target", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.CreateModel(rec, rescovReq(http.MethodPost, "/models", valid+`,"domains":["llm.example.com:8080"]}`), api.CreateModelParams{})
		rescovWant(t, rec, http.StatusUnprocessableEntity)
	})
	t.Run("creation registers the domain and regenerates the routing", func(t *testing.T) {
		a, db := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.CreateModel(rec, rescovReq(http.MethodPost, "/models", valid+`,"domains":["llm.example.com"]}`), api.CreateModelParams{})
		rescovWant(t, rec, http.StatusCreated)
		if len(db.lastArgs["CreateModelDomain"]) == 0 {
			t.Fatal("the domain never reached the insert")
		}
		var routed bool
		for _, arg := range db.lastArgs["EnqueueJob"] {
			if s, ok := arg.(string); ok && strings.Contains(s, "apply_routing") {
				routed = true
			}
		}
		if !routed {
			t.Fatalf("routing must regenerate immediately: %v", db.lastArgs["EnqueueJob"])
		}
	})
	t.Run("an fqdn already routed by the instance answers 409 (INV-002)", func(t *testing.T) {
		a, db := rescovAPI(t)
		db.errOn["CreateModelDomain"] = &pgconn.PgError{Code: "23505"}
		rec := httptest.NewRecorder()
		a.CreateModel(rec, rescovReq(http.MethodPost, "/models", valid+`,"domains":["llm.example.com"]}`), api.CreateModelParams{})
		rescovWant(t, rec, http.StatusConflict)
	})
	t.Run("update replaces the whole set and regenerates", func(t *testing.T) {
		a, db := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.UpdateModel(rec, rescovReq(http.MethodPatch, "/models/"+fixtureUUID, `{"domains":["llm2.example.com/api"]}`),
			fixtureUUID, api.UpdateModelParams{IfMatch: `"1"`})
		rescovWant(t, rec, http.StatusOK)
		if db.calls["DeleteDomainsForModel"] == 0 || len(db.lastArgs["CreateModelDomain"]) == 0 {
			t.Fatalf("the set must be replaced, not appended: calls=%v", db.calls)
		}
	})
	t.Run("an invalid element refuses the update by name", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.UpdateModel(rec, rescovReq(http.MethodPatch, "/models/"+fixtureUUID, `{"domains":["not a domain"]}`),
			fixtureUUID, api.UpdateModelParams{IfMatch: `"1"`})
		rescovWant(t, rec, http.StatusUnprocessableEntity)
	})
}

// The guard's arithmetic on its own (ADR-082 §2): a model that declares no
// fraction is unknown, not free — counting it as zero would make the sum
// optimistic exactly where the operator gave it least to work with.
func TestModelscovMemoryFractionArithmetic(t *testing.T) {
	if got := memoryFraction(nil); got != defaultMemoryFraction {
		t.Fatalf("an undeclared fraction = %v, want the engines' own default %v", got, defaultMemoryFraction)
	}
	zero := 0.0
	if got := memoryFraction(&zero); got != defaultMemoryFraction {
		t.Fatalf("a zero fraction is not a model that uses no memory: %v", got)
	}
	declared := 0.45
	if got := memoryFraction(&declared); got != declared {
		t.Fatalf("a declared fraction must be taken at its word: %v", got)
	}
	if got := percent(0.855); got != 86 {
		t.Fatalf("percent(0.855) = %d, want whole percents an operator reads", got)
	}

	// Two neighbours, one of them undeclared: the details name each with what
	// it claims, then the candidate, then the total against the budget.
	other := 0.3
	details := gpuClaimDetails(&declared, []store.ListRunningModelsOnServerRow{
		{Name: "qwen", MemoryFraction: &other},
		{Name: "unconfigured", MemoryFraction: nil},
	})
	if len(details) != 4 {
		t.Fatalf("details = %+v — one line per running model, plus the candidate and the total", details)
	}
	joined := ""
	for _, d := range details {
		joined += d.Message + "|"
	}
	for _, want := range []string{"qwen claims 30%", "unconfigured claims 90%", "this model claims 45%", "165% claimed"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("%q missing from the arithmetic: %s", want, joined)
		}
	}
}

// The omni modality (ADR-083): an orthogonal axis that changes the
// invocation, requires an explicit image, and stays immutable.
func TestModelscovOmniModality(t *testing.T) {
	create := func(t *testing.T, body string, want int) *rescovDB {
		t.Helper()
		a, db := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.CreateModel(rec, rescovReq(http.MethodPost, "/models", body), api.CreateModelParams{})
		rescovWant(t, rec, want)
		return db
	}
	base := `{"name":"music","engine":"sglang","model_id":"MiniMaxAI/MiniMax-Music3","project_uuid":"` + fixtureUUID +
		`","environment_uuid":"` + fixtureUUID + `","server_uuid":"` + fixtureUUID + `","published_port":18001`

	t.Run("an omni model without an image is refused by field name", func(t *testing.T) {
		db := create(t, base+`,"modality":"omni"}`, http.StatusUnprocessableEntity)
		if len(db.lastArgs["CreateModelRow"]) != 0 {
			t.Fatal("nothing must reach the insert")
		}
	})

	t.Run("an unknown modality is refused", func(t *testing.T) {
		create(t, base+`,"modality":"video","image":"local/sglang-omni:0.1.3"}`, http.StatusUnprocessableEntity)
	})

	t.Run("with its image, the modality reaches the insert", func(t *testing.T) {
		db := create(t, base+`,"modality":"omni","image":"local/sglang-omni:0.1.3"}`, http.StatusCreated)
		var saw bool
		for _, arg := range db.lastArgs["CreateModelRow"] {
			if m, ok := arg.(store.InferenceModality); ok && m == store.InferenceModalityOmni {
				saw = true
			}
		}
		if !saw {
			t.Fatalf("the modality never reached the insert: %v", db.lastArgs["CreateModelRow"])
		}
	})

	t.Run("a text model still resolves its per-engine default", func(t *testing.T) {
		create(t, base+`}`, http.StatusCreated)
	})

	t.Run("an omni model may not have its image cleared", func(t *testing.T) {
		a, db := rescovAPI(t)
		db.modality = string(store.InferenceModalityOmni)
		rec := httptest.NewRecorder()
		a.UpdateModel(rec, rescovReq(http.MethodPatch, "/models/"+fixtureUUID, `{"image":null}`),
			fixtureUUID, api.UpdateModelParams{IfMatch: `"1"`})
		rescovWant(t, rec, http.StatusUnprocessableEntity)

		rec = httptest.NewRecorder()
		a.UpdateModel(rec, rescovReq(http.MethodPatch, "/models/"+fixtureUUID, `{"image":"local/sglang-omni:0.1.4"}`),
			fixtureUUID, api.UpdateModelParams{IfMatch: `"1"`})
		rescovWant(t, rec, http.StatusOK)
	})

	t.Run("the preview renders the runtime the pair names", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.PreviewModelCommand(rec, rescovReq(http.MethodPost, "/models/command-preview",
			`{"engine":"sglang","modality":"omni","model_id":"MiniMaxAI/MiniMax-Music3"}`))
		rescovWant(t, rec, http.StatusOK)
		var out api.ModelCommand
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(out.Command, "sgl-omni serve --model-path MiniMaxAI/MiniMax-Music3") {
			t.Fatalf("command = %q", out.Command)
		}
	})

	t.Run("the import reads the modality back off a pasted command", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.ParseModelCommand(rec, rescovReq(http.MethodPost, "/models/parse-command",
			`{"command":"sgl-omni serve --model-path MiniMaxAI/MiniMax-Music3 --port 8000"}`))
		rescovWant(t, rec, http.StatusOK)
		var out api.ModelParseResult
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.Engine != api.ModelParseResultEngineSglang || out.Modality != api.ModelParseResultModalityOmni ||
			out.ModelId != "MiniMaxAI/MiniMax-Music3" {
			t.Fatalf("parsed = %+v", out)
		}
	})
}
