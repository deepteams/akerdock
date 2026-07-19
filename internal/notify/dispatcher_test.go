package notify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
)

type fakeNotificationStore struct {
	cursor       int64
	cursorErr    error
	events       []store.OutboxEvent
	eventsErr    error
	setCursorErr error
	cursors      []int64

	scope      store.ResolveProjectEnvironmentOfResourceRow
	scopeErr   error
	matchArg   store.MatchNotificationRulesParams
	rules      []store.MatchNotificationRulesRow
	rulesErr   error
	created    []store.CreateNotificationDeliveryParams
	createErr  map[int64]error
	deliveries map[int64]store.NotificationDelivery
	finished   []store.FinishNotificationDeliveryParams

	channels   map[int64]store.NotificationChannel
	channelErr error
	last       pgtype.Timestamptz
	lastErr    error
	suppressed int64

	digestRules       []store.ListDigestRulesDueRow
	digestRulesErr    error
	pending           []store.ListPendingDigestDeliveriesRow
	pendingErr        error
	digestFailed      []store.MarkDigestDeliveriesFailedParams
	digestSent        [][]int64
	digestSentErr     error
	digestFlushed     []int64
	digestFlushedErr  error
	finishErr         error
	digestMarkFailErr error
}

func (f *fakeNotificationStore) GetNotificationCursor(context.Context) (int64, error) {
	return f.cursor, f.cursorErr
}

func (f *fakeNotificationStore) ListOutboxEventsAfter(_ context.Context, arg store.ListOutboxEventsAfterParams) ([]store.OutboxEvent, error) {
	if arg.ID != f.cursor || arg.Limit != batchSize {
		return nil, errors.New("unexpected outbox arguments")
	}
	return f.events, f.eventsErr
}

func (f *fakeNotificationStore) SetNotificationCursor(_ context.Context, id int64) error {
	f.cursors = append(f.cursors, id)
	return f.setCursorErr
}

func (f *fakeNotificationStore) ResolveProjectEnvironmentOfResource(context.Context, pgtype.UUID) (store.ResolveProjectEnvironmentOfResourceRow, error) {
	return f.scope, f.scopeErr
}

func (f *fakeNotificationStore) MatchNotificationRules(_ context.Context, arg store.MatchNotificationRulesParams) ([]store.MatchNotificationRulesRow, error) {
	f.matchArg = arg
	return f.rules, f.rulesErr
}

func (f *fakeNotificationStore) CreateNotificationDelivery(_ context.Context, arg store.CreateNotificationDeliveryParams) (store.NotificationDelivery, error) {
	f.created = append(f.created, arg)
	if err := f.createErr[arg.RuleID]; err != nil {
		return store.NotificationDelivery{}, err
	}
	if delivery, ok := f.deliveries[arg.RuleID]; ok {
		return delivery, nil
	}
	return store.NotificationDelivery{ID: arg.RuleID}, nil
}

func (f *fakeNotificationStore) FinishNotificationDelivery(_ context.Context, arg store.FinishNotificationDeliveryParams) error {
	f.finished = append(f.finished, arg)
	return f.finishErr
}

func (f *fakeNotificationStore) GetNotificationChannelByID(context.Context, int64) (store.NotificationChannel, error) {
	if f.channelErr != nil {
		return store.NotificationChannel{}, f.channelErr
	}
	for _, channel := range f.channels {
		return channel, nil
	}
	return store.NotificationChannel{}, pgx.ErrNoRows
}

func (f *fakeNotificationStore) LastSentDelivery(context.Context, int64) (pgtype.Timestamptz, error) {
	return f.last, f.lastErr
}

func (f *fakeNotificationStore) CountSuppressedSince(context.Context, store.CountSuppressedSinceParams) (int64, error) {
	return f.suppressed, nil
}

func (f *fakeNotificationStore) ListDigestRulesDue(context.Context) ([]store.ListDigestRulesDueRow, error) {
	return f.digestRules, f.digestRulesErr
}

func (f *fakeNotificationStore) ListPendingDigestDeliveries(context.Context, int64) ([]store.ListPendingDigestDeliveriesRow, error) {
	return f.pending, f.pendingErr
}

func (f *fakeNotificationStore) MarkDigestDeliveriesFailed(_ context.Context, arg store.MarkDigestDeliveriesFailedParams) error {
	f.digestFailed = append(f.digestFailed, arg)
	return f.digestMarkFailErr
}

func (f *fakeNotificationStore) MarkDigestDeliveriesSent(_ context.Context, ids []int64) error {
	f.digestSent = append(f.digestSent, append([]int64(nil), ids...))
	return f.digestSentErr
}

func (f *fakeNotificationStore) SetRuleDigestFlushed(_ context.Context, id int64) error {
	f.digestFlushed = append(f.digestFlushed, id)
	return f.digestFlushedErr
}

func testKeyring(t *testing.T) *envelope.Keyring {
	t.Helper()
	line := "1:" + base64.StdEncoding.EncodeToString(make([]byte, 32))
	keyring, err := envelope.Parse([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	return keyring
}

func testUUID() pgtype.UUID {
	return pguuid.MustParse("11111111-2222-4333-8444-555555555555")
}

func encryptedChannel(t *testing.T, keyring *envelope.Keyring, kind store.NotificationChannelKind, cfg Config) store.NotificationChannel {
	t.Helper()
	uuid := testUUID()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := keyring.Encrypt("notification_channels", "config_enc", pguuid.String(uuid), raw)
	if err != nil {
		t.Fatal(err)
	}
	return store.NotificationChannel{
		ID: 7, Uuid: uuid, Kind: kind, Name: "on-call", ConfigEnc: ciphertext,
	}
}

func testDispatcher(t *testing.T, database *fakeNotificationStore, channel store.NotificationChannel) *Dispatcher {
	t.Helper()
	if database.channels == nil {
		database.channels = map[int64]store.NotificationChannel{channel.ID: channel}
	}
	return &Dispatcher{
		Store: database, Keyring: testKeyring(t), Sender: New(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestDispatchReadsRoutesAndAdvancesCursor(t *testing.T) {
	keyring := testKeyring(t)
	var received Event
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode webhook: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	team := testUUID()
	resource := pguuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	event := store.OutboxEvent{
		ID: 2, EventType: "deployment.failed.v1", TeamUuid: team, ResourceUuid: resource,
		OccurredAt: pgtype.Timestamptz{Time: time.Unix(10, 0), Valid: true},
		Payload:    []byte(`{"deployment":"blue"}`),
	}
	channel := encryptedChannel(t, keyring, store.NotificationChannelKindWebhook, Config{URL: server.URL})
	database := &fakeNotificationStore{
		events: []store.OutboxEvent{{ID: 1}, event},
		scope:  store.ResolveProjectEnvironmentOfResourceRow{ProjectID: 12, EnvironmentID: 34},
		rules: []store.MatchNotificationRulesRow{
			{ID: 8, ChannelID: channel.ID, MinSeverity: store.NotificationSeverityCritical},
			{ID: 9, ChannelID: channel.ID, MinSeverity: store.NotificationSeverityWarning},
		},
		createErr: map[int64]error{8: pgx.ErrNoRows},
		deliveries: map[int64]store.NotificationDelivery{
			9: {ID: 90},
		},
		channels: map[int64]store.NotificationChannel{channel.ID: channel},
	}
	dispatcher := &Dispatcher{
		Store: database, Keyring: keyring, Sender: New(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	dispatcher.Dispatch(context.Background())

	if !reflect.DeepEqual(database.cursors, []int64{1, 2}) {
		t.Fatalf("advanced cursors = %v", database.cursors)
	}
	if database.matchArg.ProjectID == nil || *database.matchArg.ProjectID != 12 ||
		database.matchArg.EnvironmentID == nil || *database.matchArg.EnvironmentID != 34 {
		t.Fatalf("resolved scope was not passed to rules: %#v", database.matchArg)
	}
	if len(database.created) != 2 {
		t.Fatalf("created deliveries = %d, want 2", len(database.created))
	}
	if len(database.finished) != 1 || database.finished[0].Status != store.NotificationDeliveryStatusSent {
		t.Fatalf("finished = %#v", database.finished)
	}
	if received.Type != event.EventType || received.TeamUUID == "" || received.Resource == "" ||
		received.Payload["deployment"] != "blue" {
		t.Fatalf("received event = %#v", received)
	}
}

func TestDispatchFailureAndCancellationBranches(t *testing.T) {
	errBoom := errors.New("boom")
	cases := []struct {
		name string
		db   *fakeNotificationStore
		ctx  func() context.Context
		want []int64
	}{
		{"cursor read", &fakeNotificationStore{cursorErr: errBoom}, context.Background, nil},
		{"outbox read", &fakeNotificationStore{eventsErr: errBoom}, context.Background, nil},
		{"rules", &fakeNotificationStore{
			events: []store.OutboxEvent{{ID: 1, TeamUuid: testUUID()}}, rulesErr: errBoom,
		}, context.Background, nil},
		{"create", &fakeNotificationStore{
			events:    []store.OutboxEvent{{ID: 1, TeamUuid: testUUID()}},
			rules:     []store.MatchNotificationRulesRow{{ID: 2}},
			createErr: map[int64]error{2: errBoom},
		}, context.Background, nil},
		{"advance", &fakeNotificationStore{
			events: []store.OutboxEvent{{ID: 1}}, setCursorErr: errBoom,
		}, context.Background, []int64{1}},
		{"cancelled", &fakeNotificationStore{
			events: []store.OutboxEvent{{ID: 1}},
		}, func() context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dispatcher := testDispatcher(t, tc.db, store.NotificationChannel{})
			dispatcher.Dispatch(tc.ctx())
			if !reflect.DeepEqual(tc.db.cursors, tc.want) {
				t.Fatalf("cursors = %v, want %v", tc.db.cursors, tc.want)
			}
		})
	}
}

func TestDispatchSkipsBelowThresholdAndIgnoresScopeLookupFailure(t *testing.T) {
	database := &fakeNotificationStore{
		events: []store.OutboxEvent{{
			ID: 3, EventType: "deployment.succeeded.v1",
			TeamUuid: testUUID(), ResourceUuid: testUUID(),
		}},
		scopeErr: errors.New("resource disappeared"),
		rules: []store.MatchNotificationRulesRow{{
			ID: 4, MinSeverity: store.NotificationSeverityWarning,
		}},
	}
	testDispatcher(t, database, store.NotificationChannel{}).Dispatch(context.Background())
	if database.matchArg.ProjectID != nil || database.matchArg.EnvironmentID != nil {
		t.Fatalf("failed scope lookup must stay team-wide: %#v", database.matchArg)
	}
	if len(database.created) != 0 || !reflect.DeepEqual(database.cursors, []int64{3}) {
		t.Fatalf("created=%d cursors=%v", len(database.created), database.cursors)
	}
}

func TestDeliverSuppressionDigestAndFailures(t *testing.T) {
	keyring := testKeyring(t)
	okServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer okServer.Close()
	channel := encryptedChannel(t, keyring, store.NotificationChannelKindWebhook, Config{URL: okServer.URL})
	event := store.OutboxEvent{EventType: "deployment.succeeded.v1"}
	rule := store.MatchNotificationRulesRow{ID: 1, ChannelID: channel.ID}
	delivery := store.NotificationDelivery{ID: 10}

	t.Run("debounced", func(t *testing.T) {
		database := &fakeNotificationStore{
			last: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		}
		dispatcher := testDispatcher(t, database, channel)
		rule := rule
		rule.DebounceSeconds = 60
		dispatcher.deliver(context.Background(), rule, delivery, event, SeverityInfo)
		if len(database.finished) != 1 ||
			database.finished[0].Status != store.NotificationDeliveryStatusSuppressed ||
			database.finished[0].SuppressedReason == nil {
			t.Fatalf("finish = %#v", database.finished)
		}
	})

	t.Run("digest stays pending", func(t *testing.T) {
		database := &fakeNotificationStore{}
		dispatcher := testDispatcher(t, database, channel)
		rule := rule
		rule.DigestEnabled = true
		dispatcher.deliver(context.Background(), rule, delivery, event, SeverityWarning)
		if len(database.finished) != 0 {
			t.Fatalf("pending digest was finished: %#v", database.finished)
		}
	})

	t.Run("channel lookup", func(t *testing.T) {
		database := &fakeNotificationStore{channelErr: errors.New("missing")}
		dispatcher := testDispatcher(t, database, channel)
		dispatcher.deliver(context.Background(), rule, delivery, event, SeverityInfo)
		assertFinishedStatus(t, database, store.NotificationDeliveryStatusFailed)
	})

	t.Run("decrypt", func(t *testing.T) {
		broken := channel
		broken.ConfigEnc = []byte("broken")
		database := &fakeNotificationStore{}
		dispatcher := testDispatcher(t, database, broken)
		dispatcher.deliver(context.Background(), rule, delivery, event, SeverityInfo)
		assertFinishedStatus(t, database, store.NotificationDeliveryStatusFailed)
	})

	t.Run("provider", func(t *testing.T) {
		failed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "rejected", http.StatusBadGateway)
		}))
		defer failed.Close()
		badChannel := encryptedChannel(t, keyring, store.NotificationChannelKindWebhook, Config{URL: failed.URL})
		database := &fakeNotificationStore{}
		dispatcher := testDispatcher(t, database, badChannel)
		dispatcher.deliver(context.Background(), rule, delivery, event, SeverityInfo)
		assertFinishedStatus(t, database, store.NotificationDeliveryStatusFailed)
		if database.finished[0].LastError == nil {
			t.Fatal("provider error was not recorded")
		}
	})

	t.Run("critical bypasses noise and counts suppressed", func(t *testing.T) {
		database := &fakeNotificationStore{
			last:       pgtype.Timestamptz{Time: time.Now(), Valid: true},
			suppressed: 3,
		}
		dispatcher := testDispatcher(t, database, channel)
		rule := rule
		rule.DebounceSeconds = 3600
		rule.DigestEnabled = true
		dispatcher.deliver(context.Background(), rule, delivery,
			store.OutboxEvent{EventType: "deployment.failed.v1"}, SeverityCritical)
		assertFinishedStatus(t, database, store.NotificationDeliveryStatusSent)
	})
}

func assertFinishedStatus(t *testing.T, database *fakeNotificationStore, want store.NotificationDeliveryStatus) {
	t.Helper()
	if len(database.finished) != 1 || database.finished[0].Status != want {
		t.Fatalf("finish = %#v, want %s", database.finished, want)
	}
}

func TestSuppressionReasonAndConfig(t *testing.T) {
	database := &fakeNotificationStore{lastErr: errors.New("no history")}
	dispatcher := testDispatcher(t, database, store.NotificationChannel{})
	if got := dispatcher.suppressionReason(context.Background(),
		store.MatchNotificationRulesRow{DebounceSeconds: 10}, SeverityInfo); got != nil {
		t.Fatalf("history error must not suppress: %q", *got)
	}
	if got := dispatcher.suppressionReason(context.Background(),
		quietRule("00:00:00", "23:59:59"), SeverityInfo); got == nil || *got != "quiet hours" {
		t.Fatalf("quiet-hours reason = %v", got)
	}

	keyring := testKeyring(t)
	channel := encryptedChannel(t, keyring, store.NotificationChannelKindWebhook, Config{URL: "https://example.test"})
	dispatcher.Keyring = keyring
	cfg, err := dispatcher.config(channel)
	if err != nil || cfg.URL != "https://example.test" {
		t.Fatalf("config = %#v, %v", cfg, err)
	}
	channel.ConfigEnc, err = keyring.Encrypt(
		"notification_channels", "config_enc", pguuid.String(channel.Uuid), []byte("{"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dispatcher.config(channel); err == nil {
		t.Fatal("invalid configuration JSON was accepted")
	}
}

func TestBuildEventWithMalformedPayload(t *testing.T) {
	event := store.OutboxEvent{
		EventType: "x.v1", Payload: []byte("{"), OccurredAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
	got := buildEvent(event, SeverityInfo, 2)
	if got.Payload != nil || got.TeamUUID != "" || got.Resource != "" || got.Suppressed != 2 {
		t.Fatalf("buildEvent = %#v", got)
	}
}

func TestFlushDigests(t *testing.T) {
	keyring := testKeyring(t)
	var sent Event
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&sent)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	channel := encryptedChannel(t, keyring, store.NotificationChannelKindWebhook, Config{URL: server.URL})
	database := &fakeNotificationStore{
		digestRules: []store.ListDigestRulesDueRow{{ID: 4, ChannelID: channel.ID, EventType: "deployment.succeeded.v1"}},
		pending: []store.ListPendingDigestDeliveriesRow{
			{ID: 11, EventType: "deployment.succeeded.v1", OccurredAt: pgtype.Timestamptz{Time: time.Unix(20, 0), Valid: true}},
			{ID: 12, EventType: "deployment.failed.v1", OccurredAt: pgtype.Timestamptz{Time: time.Unix(10, 0), Valid: true}},
		},
		channels: map[int64]store.NotificationChannel{channel.ID: channel},
	}
	dispatcher := &Dispatcher{
		Store: database, Keyring: keyring, Sender: New(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	dispatcher.FlushDigests(context.Background())

	if !reflect.DeepEqual(database.digestSent, [][]int64{{11, 12}}) ||
		!reflect.DeepEqual(database.digestFlushed, []int64{4}) {
		t.Fatalf("sent=%v flushed=%v", database.digestSent, database.digestFlushed)
	}
	if sent.Type != "notification.digest.v1" || sent.Suppressed != 1 ||
		sent.Payload["total"] != float64(2) {
		t.Fatalf("digest = %#v", sent)
	}
}

func TestFlushDigestFailureBranches(t *testing.T) {
	errBoom := errors.New("boom")
	dispatcherFor := func(t *testing.T, database *fakeNotificationStore, channel store.NotificationChannel) *Dispatcher {
		t.Helper()
		return testDispatcher(t, database, channel)
	}
	t.Run("list rules", func(t *testing.T) {
		database := &fakeNotificationStore{digestRulesErr: errBoom}
		dispatcherFor(t, database, store.NotificationChannel{}).FlushDigests(context.Background())
	})
	t.Run("cancelled", func(t *testing.T) {
		database := &fakeNotificationStore{digestRules: []store.ListDigestRulesDueRow{{ID: 1}}}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		dispatcherFor(t, database, store.NotificationChannel{}).FlushDigests(ctx)
	})
	t.Run("pending error and empty", func(t *testing.T) {
		for _, database := range []*fakeNotificationStore{{pendingErr: errBoom}, {}} {
			err := dispatcherFor(t, database, store.NotificationChannel{}).
				flushDigest(context.Background(), store.ListDigestRulesDueRow{ID: 1})
			if database.pendingErr != nil && !errors.Is(err, errBoom) {
				t.Fatalf("error = %v", err)
			}
			if database.pendingErr == nil && err != nil {
				t.Fatalf("empty digest error = %v", err)
			}
		}
	})
	t.Run("channel and config", func(t *testing.T) {
		pending := []store.ListPendingDigestDeliveriesRow{{ID: 1}}
		database := &fakeNotificationStore{pending: pending, channelErr: errBoom}
		if err := dispatcherFor(t, database, store.NotificationChannel{}).
			flushDigest(context.Background(), store.ListDigestRulesDueRow{}); !errors.Is(err, errBoom) {
			t.Fatalf("channel error = %v", err)
		}
		database = &fakeNotificationStore{pending: pending}
		broken := store.NotificationChannel{ID: 2, Uuid: testUUID(), ConfigEnc: []byte("bad")}
		if err := dispatcherFor(t, database, broken).
			flushDigest(context.Background(), store.ListDigestRulesDueRow{ChannelID: 2}); err == nil {
			t.Fatal("invalid encrypted configuration was accepted")
		}
	})
	t.Run("provider failure is recorded", func(t *testing.T) {
		failed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "no", http.StatusBadGateway)
		}))
		defer failed.Close()
		keyring := testKeyring(t)
		channel := encryptedChannel(t, keyring, store.NotificationChannelKindWebhook, Config{URL: failed.URL})
		database := &fakeNotificationStore{
			pending:  []store.ListPendingDigestDeliveriesRow{{ID: 1}},
			channels: map[int64]store.NotificationChannel{channel.ID: channel},
		}
		dispatcher := &Dispatcher{
			Store: database, Keyring: keyring, Sender: New(),
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		if err := dispatcher.flushDigest(context.Background(),
			store.ListDigestRulesDueRow{ID: 2, ChannelID: channel.ID}); err == nil {
			t.Fatal("provider failure was hidden")
		}
		if len(database.digestFailed) != 1 || database.digestFailed[0].LastError == nil {
			t.Fatalf("failed = %#v", database.digestFailed)
		}
	})
	t.Run("mark sent", func(t *testing.T) {
		keyring := testKeyring(t)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
		defer server.Close()
		channel := encryptedChannel(t, keyring, store.NotificationChannelKindWebhook, Config{URL: server.URL})
		database := &fakeNotificationStore{
			pending:       []store.ListPendingDigestDeliveriesRow{{ID: 1}},
			channels:      map[int64]store.NotificationChannel{channel.ID: channel},
			digestSentErr: errBoom,
		}
		dispatcher := &Dispatcher{
			Store: database, Keyring: keyring, Sender: New(),
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		if err := dispatcher.flushDigest(context.Background(),
			store.ListDigestRulesDueRow{ChannelID: channel.ID}); !errors.Is(err, errBoom) {
			t.Fatalf("mark sent error = %v", err)
		}
	})
}
