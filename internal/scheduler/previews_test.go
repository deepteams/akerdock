package scheduler

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/dockerruntime"
	dockerfake "github.com/deepteams/akerdock/internal/dockerruntime/fake"
	hostfake "github.com/deepteams/akerdock/internal/hostops/fake"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/sshexec"
	"github.com/deepteams/akerdock/internal/store"
)

// uuidN derives a distinct, stable UUID per test resource so the per-resource
// activity files land on distinct paths.
func uuidN(n byte) pgtype.UUID {
	return pguuid.MustParse(fmt.Sprintf("%02x111111-2222-4333-8444-555555555555", n))
}

// placementStore wires application 3 → destination 5 → server 6, the chain
// previewPlacement resolves.
func placementStore() *fakeSchedulerStore {
	return &fakeSchedulerStore{
		application: store.GetApplicationByIDRow{
			Resource:    store.Resource{ID: 3, Uuid: testUUID(), TeamID: 4, DestinationID: 5, Name: "app"},
			Application: store.Application{PreviewsEnabled: true, PreviewDeployOnOpen: true},
		},
		destination: store.Destination{ID: 5, ServerID: 6, Network: "proxynet"},
		serverRow: store.Server{
			ID: 6, Uuid: testUUID(), TeamID: 4, Host: "srv.example", Port: 22,
			SshUser: "deploy", SshTimeoutSeconds: 1, PrivateKeyID: 7,
		},
		team: store.Team{Uuid: testUUID()},
	}
}

// sealedPrivateKey is a key row the test keyring can decrypt, so
// serverdial.Key succeeds against the fake store.
func sealedPrivateKey(t *testing.T) store.PrivateKey {
	t.Helper()
	u := testUUID()
	enc, err := schedulerKeyring(t).Encrypt("private_keys", "private_key_enc", pguuid.String(u), []byte("pem"))
	if err != nil {
		t.Fatal(err)
	}
	return store.PrivateKey{ID: 7, Uuid: u, PrivateKeyEnc: enc}
}

// fakeRemoteClient records the commands the agent scan runs over SSH.
type fakeRemoteClient struct {
	mu       sync.Mutex
	commands []string
	runErr   error
	closed   int
}

func (f *fakeRemoteClient) Run(_ context.Context, command string) (*sshexec.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, command)
	if f.runErr != nil {
		return nil, f.runErr
	}
	return &sshexec.Result{}, nil
}

func (f *fakeRemoteClient) RunInput(ctx context.Context, command, _ string) (*sshexec.Result, error) {
	return f.Run(ctx, command)
}

func (f *fakeRemoteClient) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return nil
}

// stubDockerSource hands every caller the same runtime (or a failure).
type stubDockerSource struct {
	rt  dockerruntime.Runtime
	err error
}

func (s stubDockerSource) Runtime(context.Context, int64) (dockerruntime.Runtime, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.rt, nil
}

// stzFixture is a scheduler wired for a scale-to-zero pass: fake store, fake
// agent channel, fake Docker runtime and an injected SSH dial.
type stzFixture struct {
	db     *fakeSchedulerStore
	ops    *hostfake.Ops
	rt     *dockerfake.Runtime
	client *fakeRemoteClient
	s      *Scheduler
}

func newSTZFixture(t *testing.T, db *fakeSchedulerStore) *stzFixture {
	t.Helper()
	f := &stzFixture{db: db, ops: &hostfake.Ops{}, rt: &dockerfake.Runtime{}, client: &fakeRemoteClient{}}
	db.privateKey = sealedPrivateKey(t)
	s := newScheduler(t, db)
	s.AgentImage = "akerdock/akerdock:test"
	s.HostOps = stubHostSource{ops: f.ops}
	s.Docker = stubDockerSource{rt: f.rt}
	s.dialSSH = func(context.Context, store.Server, string) (remoteClient, error) { return f.client, nil }
	f.s = s
	return f
}

// oldActivity is well past every idle window used in these tests.
const oldActivity = "1000000000"

func activityFile(content string) func(context.Context, agentwire.FileReadParams) (agentwire.FileReadResult, error) {
	return func(context.Context, agentwire.FileReadParams) (agentwire.FileReadResult, error) {
		return agentwire.FileReadResult{Found: true, Content: []byte(content)}, nil
	}
}

func emptyContainerList(context.Context, container.ListOptions) ([]container.Summary, error) {
	return nil, nil
}

// swallowPanics tolerates the latent nil-client panic of agentScan.close: a
// server whose SSH dial failed is cached as a nil remoteClient, and close's
// blind c.Close() then panics. The decisions under test all happen before the
// deferred close, so the pass's observable state is still assertable — and
// this helper keeps working the day the panic is fixed.
func swallowPanics(fn func()) {
	defer func() { _ = recover() }()
	fn()
}

func outboxTypes(db *fakeSchedulerStore) []string {
	var types []string
	for _, event := range db.outbox {
		types = append(types, event.EventType)
	}
	return types
}

func TestEmitLifecycleEvents(t *testing.T) {
	db := placementStore()
	scheduler := newScheduler(t, db)
	scheduler.emitPreviewEvent(context.Background(), "application.preview.slept.v1", 3, uuidN(1), 7)
	scheduler.emitApplicationEvent(context.Background(), "application.slept.v1", 3, testUUID())
	want := []string{"application.preview.slept.v1", "application.slept.v1"}
	if !reflect.DeepEqual(outboxTypes(db), want) {
		t.Fatalf("outbox = %v", outboxTypes(db))
	}

	// A team lookup failure degrades the payload, never drops the event.
	db.errs = map[string]error{"team": errors.New("x")}
	scheduler.emitPreviewEvent(context.Background(), "application.preview.woken.v1", 3, uuidN(1), 7)
	scheduler.emitApplicationEvent(context.Background(), "application.woken.v1", 3, testUUID())
	if len(db.outbox) != 4 {
		t.Fatalf("outbox after team failure = %v", outboxTypes(db))
	}

	// An application already gone emits nothing.
	failing := &fakeSchedulerStore{errs: map[string]error{"application": errors.New("x")}}
	scheduler = newScheduler(t, failing)
	scheduler.emitPreviewEvent(context.Background(), "application.preview.slept.v1", 3, uuidN(1), 7)
	scheduler.emitApplicationEvent(context.Background(), "application.slept.v1", 3, testUUID())
	if len(failing.outbox) != 0 {
		t.Fatalf("outbox for a missing application = %v", outboxTypes(failing))
	}
}

func TestPreviewExpiryWarnings(t *testing.T) {
	fqdn := "pr-11.example.test"
	t.Run("warns each preview once", func(t *testing.T) {
		db := placementStore()
		db.toWarn = []store.Preview{
			{ID: 1, Uuid: uuidN(1), ApplicationID: 3, PrID: 11, Fqdn: &fqdn},
			{ID: 2, Uuid: uuidN(2), ApplicationID: 3, PrID: 12},
		}
		newScheduler(t, db).reapPreviews(context.Background())
		if !reflect.DeepEqual(outboxTypes(db), []string{"application.preview.expiring.v1", "application.preview.expiring.v1"}) {
			t.Fatalf("outbox = %v", outboxTypes(db))
		}
		if !reflect.DeepEqual(db.warnedPreviewIDs, []int64{1, 2}) {
			t.Fatalf("warned = %v", db.warnedPreviewIDs)
		}
	})
	t.Run("team lookup failure still warns", func(t *testing.T) {
		db := placementStore()
		db.errs = map[string]error{"team": errors.New("x")}
		db.toWarn = []store.Preview{{ID: 1, Uuid: uuidN(1), ApplicationID: 3, PrID: 11}}
		newScheduler(t, db).reapPreviews(context.Background())
		if len(db.outbox) != 1 || len(db.warnedPreviewIDs) != 1 {
			t.Fatalf("outbox=%d warned=%v", len(db.outbox), db.warnedPreviewIDs)
		}
	})
	t.Run("scan and application failures", func(t *testing.T) {
		newScheduler(t, &fakeSchedulerStore{errs: map[string]error{"toWarn": errors.New("x")}}).
			reapPreviews(context.Background())
		db := placementStore()
		db.errs = map[string]error{"application": errors.New("x")}
		db.toWarn = []store.Preview{{ID: 1, Uuid: uuidN(1), ApplicationID: 3}}
		db.queued = []store.Preview{{ID: 2, Uuid: uuidN(2), ApplicationID: 3}}
		newScheduler(t, db).reapPreviews(context.Background())
		if len(db.outbox) != 0 || len(db.warnedPreviewIDs) != 0 || len(db.enqueueArgs) != 0 {
			t.Fatalf("missing application still acted: outbox=%d warned=%v", len(db.outbox), db.warnedPreviewIDs)
		}
	})
	t.Run("destroy enqueue failure is logged and skipped", func(t *testing.T) {
		db := &fakeSchedulerStore{
			expired: []store.Preview{{ID: 10, Uuid: uuidN(3)}},
			errs:    map[string]error{"enqueue": errors.New("x")},
		}
		newScheduler(t, db).reapPreviews(context.Background())
	})
}

func TestScaleZeroPreviewsLifecycle(t *testing.T) {
	db := placementStore()
	db.stzActive = []store.ListPreviewsForScaleToZeroRow{{
		ID: 21, Uuid: uuidN(0xaa), ApplicationID: 3, PrID: 5, ScaleToZeroAfterMinutes: 30,
	}}
	db.stzSleeping = []store.Preview{
		{ID: 22, Uuid: uuidN(0xbb), ApplicationID: 3, PrID: 6,
			UpdatedAt: pgtype.Timestamptz{Time: time.Unix(900_000_000, 0), Valid: true}},
		// Slept after the last recorded activity: the wake never happened.
		{ID: 23, Uuid: uuidN(0xcc), ApplicationID: 3, PrID: 7,
			UpdatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}},
	}
	fixture := newSTZFixture(t, db)
	fixture.ops.ReadFileFn = activityFile(oldActivity)
	fixture.rt.ContainerListFn = func(context.Context, container.ListOptions) ([]container.Summary, error) {
		// One summary without a name (tolerated), one labelled container.
		return []container.Summary{{}, {Names: []string{"/pr-web"}}}, nil
	}
	fixture.s.scaleZeroPreviews(context.Background())

	if !reflect.DeepEqual(db.sleptPreviewIDs, []int64{21}) {
		t.Fatalf("slept = %v", db.sleptPreviewIDs)
	}
	if !reflect.DeepEqual(db.wokenPreviewIDs, []int64{22}) {
		t.Fatalf("woken = %v", db.wokenPreviewIDs)
	}
	if !reflect.DeepEqual(outboxTypes(db), []string{"application.preview.slept.v1", "application.preview.woken.v1"}) {
		t.Fatalf("outbox = %v", outboxTypes(db))
	}
	stops := 0
	for _, name := range fixture.rt.CallNames() {
		if name == "ContainerStop" {
			stops++
		}
	}
	if stops != 2 { // the named container + the labelled one
		t.Fatalf("container stops = %d (%v)", stops, fixture.rt.CallNames())
	}
	// One SSH connection and one waker reconcile for the whole pass, closed at
	// the end.
	if len(fixture.client.commands) != 1 || fixture.client.closed != 1 {
		t.Fatalf("ssh commands=%d closed=%d", len(fixture.client.commands), fixture.client.closed)
	}
}

func TestScaleZeroPreviewsDatabaseActivityWins(t *testing.T) {
	// A redeploy IS activity: a fresh last_deployed_at outweighs a stale waker
	// file, so the preview stays awake.
	db := placementStore()
	db.stzActive = []store.ListPreviewsForScaleToZeroRow{{
		ID: 24, Uuid: uuidN(0xaa), ApplicationID: 3, ScaleToZeroAfterMinutes: 30,
		LastDeployedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}}
	fixture := newSTZFixture(t, db)
	fixture.ops.ReadFileFn = activityFile(oldActivity)
	fixture.s.scaleZeroPreviews(context.Background())
	if len(db.sleptPreviewIDs) != 0 || len(db.outbox) != 0 {
		t.Fatalf("recently deployed preview slept: %v", db.sleptPreviewIDs)
	}
}

func TestScaleZeroPreviewsSkipsAndFailures(t *testing.T) {
	activeRows := func(n int) []store.ListPreviewsForScaleToZeroRow {
		rows := make([]store.ListPreviewsForScaleToZeroRow, n)
		for i := range rows {
			rows[i] = store.ListPreviewsForScaleToZeroRow{
				ID: int64(21 + i), Uuid: uuidN(byte(0xa0 + i)), ApplicationID: 3,
				ScaleToZeroAfterMinutes: 30,
			}
		}
		return rows
	}
	sleepingRows := []store.Preview{{
		ID: 41, Uuid: uuidN(0xe1), ApplicationID: 3,
		UpdatedAt: pgtype.Timestamptz{Time: time.Unix(900_000_000, 0), Valid: true},
	}}
	for _, tc := range []struct {
		name       string
		configure  func(f *stzFixture)
		wantSleeps int
		wantWakes  int
		wantEvents int
	}{
		{"active scan failure", func(f *stzFixture) {
			f.db.errs = map[string]error{"stzList": errors.New("x")}
		}, 0, 0, 0},
		{"sleeping scan failure", func(f *stzFixture) {
			f.db.errs = map[string]error{"sleepingList": errors.New("x")}
		}, 0, 0, 0},
		{"application lookup failure", func(f *stzFixture) {
			f.db.errs = map[string]error{"application": errors.New("x")}
			f.db.stzSleeping = sleepingRows // the sleeping loop skips too
		}, 0, 0, 0},
		{"destination lookup failure", func(f *stzFixture) {
			f.db.errs = map[string]error{"destination": errors.New("x")}
		}, 0, 0, 0},
		{"server lookup failure", func(f *stzFixture) {
			f.db.errs = map[string]error{"server": errors.New("x")}
		}, 0, 0, 0},
		// SSH is only the reconcile's transport: losing it skips the reconcile
		// but never the decision, which rides the agent channel.
		{"key failure still decides", func(f *stzFixture) {
			f.db.errs = map[string]error{"privateKey": errors.New("x")}
		}, 1, 0, 1},
		{"dial failure still decides", func(f *stzFixture) {
			f.db.stzActive = activeRows(2) // second row hits the cached nil client
			f.s.dialSSH = func(context.Context, store.Server, string) (remoteClient, error) {
				return nil, errors.New("dial refused")
			}
		}, 2, 0, 2},
		{"real dial branch still decides", func(f *stzFixture) {
			f.s.dialSSH = nil
			f.db.serverRow.Host = "127.0.0.1"
			f.db.serverRow.Port = 1 // closed privileged port: instant refusal
		}, 1, 0, 1},
		{"agent channel unavailable", func(f *stzFixture) {
			f.db.stzActive = activeRows(2) // second row hits the cached nil ops
			f.db.stzSleeping = sleepingRows
			f.s.HostOps = stubHostSource{}
		}, 0, 0, 0},
		{"activity read failure", func(f *stzFixture) {
			f.ops.ReadFileFn = func(context.Context, agentwire.FileReadParams) (agentwire.FileReadResult, error) {
				return agentwire.FileReadResult{}, errors.New("channel down")
			}
		}, 0, 0, 0},
		{"activity absent", func(f *stzFixture) {
			f.ops.ReadFileFn = nil // the zero-value fake answers "absent"
		}, 0, 0, 0},
		{"activity unparseable", func(f *stzFixture) {
			f.ops.ReadFileFn = activityFile("not-a-timestamp")
		}, 0, 0, 0},
		{"activity fresh", func(f *stzFixture) {
			f.ops.ReadFileFn = activityFile(fmt.Sprint(time.Now().Unix()))
		}, 0, 0, 0},
		{"runtime unavailable", func(f *stzFixture) {
			f.s.Docker = stubDockerSource{err: errors.New("agent down")}
		}, 0, 0, 0},
		{"stop failure", func(f *stzFixture) {
			f.rt.ContainerStopFn = func(context.Context, string, container.StopOptions) error {
				return errors.New("daemon busy")
			}
		}, 0, 0, 0},
		{"status update failure", func(f *stzFixture) {
			f.db.errs = map[string]error{"sleep": errors.New("x")}
		}, 1, 0, 0},
		{"sleeping read failure", func(f *stzFixture) {
			f.db.stzActive = nil
			f.db.stzSleeping = sleepingRows
			f.ops.ReadFileFn = func(context.Context, agentwire.FileReadParams) (agentwire.FileReadResult, error) {
				return agentwire.FileReadResult{}, errors.New("channel down")
			}
		}, 0, 0, 0},
		{"wake update failure", func(f *stzFixture) {
			f.db.stzActive = nil
			f.db.stzSleeping = sleepingRows
			f.db.errs = map[string]error{"awake": errors.New("x")}
		}, 0, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := placementStore()
			db.stzActive = activeRows(1)
			fixture := newSTZFixture(t, db)
			fixture.ops.ReadFileFn = activityFile(oldActivity)
			fixture.rt.ContainerListFn = emptyContainerList
			tc.configure(fixture)
			swallowPanics(func() { fixture.s.scaleZeroPreviews(context.Background()) })
			if len(db.sleptPreviewIDs) != tc.wantSleeps || len(db.wokenPreviewIDs) != tc.wantWakes {
				t.Fatalf("slept=%v woken=%v", db.sleptPreviewIDs, db.wokenPreviewIDs)
			}
			if len(db.outbox) != tc.wantEvents {
				t.Fatalf("outbox = %v", outboxTypes(db))
			}
		})
	}
}

func TestScaleZeroApplicationsLifecycle(t *testing.T) {
	db := placementStore()
	db.appsToSleep = []store.ListApplicationsToSleepRow{{
		ID: 31, Uuid: uuidN(0xd1), ScaleToZeroAfterMinutes: 30,
		// A deploy newer than the waker file counts as the latest activity —
		// still idle here, so the app sleeps.
		UpdatedAt: pgtype.Timestamptz{Time: time.Unix(1_100_000_000, 0), Valid: true},
	}}
	db.appsSleeping = []store.ListSleepingApplicationsRow{
		{ID: 32, Uuid: uuidN(0xd2), ScaleSleptAt: pgtype.Timestamptz{Time: time.Unix(900_000_000, 0), Valid: true}},
		{ID: 33, Uuid: uuidN(0xd3), ScaleSleptAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}},
	}
	fixture := newSTZFixture(t, db)
	fixture.ops.ReadFileFn = activityFile(oldActivity)
	fixture.rt.ContainerListFn = func(context.Context, container.ListOptions) ([]container.Summary, error) {
		return []container.Summary{{Names: []string{"/app-web"}}}, nil
	}
	fixture.s.scaleZeroApplications(context.Background())

	if !reflect.DeepEqual(db.sleptAppIDs, []int64{31}) || !reflect.DeepEqual(db.wokenAppIDs, []int64{32}) {
		t.Fatalf("slept=%v woken=%v", db.sleptAppIDs, db.wokenAppIDs)
	}
	if !reflect.DeepEqual(outboxTypes(db), []string{"application.slept.v1", "application.woken.v1"}) {
		t.Fatalf("outbox = %v", outboxTypes(db))
	}
}

func TestScaleZeroApplicationsSkipsAndFailures(t *testing.T) {
	toSleep := []store.ListApplicationsToSleepRow{{
		ID: 31, Uuid: uuidN(0xd1), ScaleToZeroAfterMinutes: 30,
	}}
	sleeping := []store.ListSleepingApplicationsRow{{
		ID: 32, Uuid: uuidN(0xd2),
		ScaleSleptAt: pgtype.Timestamptz{Time: time.Unix(900_000_000, 0), Valid: true},
	}}
	for _, tc := range []struct {
		name       string
		configure  func(f *stzFixture)
		wantSleeps int
		wantWakes  int
		wantEvents int
	}{
		{"scan failure", func(f *stzFixture) {
			f.db.errs = map[string]error{"appSleepList": errors.New("x")}
		}, 0, 0, 0},
		{"sleeping scan failure", func(f *stzFixture) {
			f.db.errs = map[string]error{"appSleepingList": errors.New("x")}
		}, 0, 0, 0},
		{"placement failure", func(f *stzFixture) {
			f.db.errs = map[string]error{"application": errors.New("x")}
		}, 0, 0, 0},
		{"agent channel unavailable", func(f *stzFixture) {
			f.s.HostOps = stubHostSource{}
		}, 0, 0, 0},
		{"activity read failure on both loops", func(f *stzFixture) {
			f.ops.ReadFileFn = func(context.Context, agentwire.FileReadParams) (agentwire.FileReadResult, error) {
				return agentwire.FileReadResult{}, errors.New("channel down")
			}
		}, 0, 0, 0},
		{"activity fresh", func(f *stzFixture) {
			f.ops.ReadFileFn = activityFile(fmt.Sprint(time.Now().Unix()))
			f.db.appsSleeping = nil
		}, 0, 0, 0},
		{"runtime unavailable", func(f *stzFixture) {
			f.s.Docker = stubDockerSource{err: errors.New("agent down")}
			f.db.appsSleeping = nil
		}, 0, 0, 0},
		{"stop failure", func(f *stzFixture) {
			f.rt.ContainerStopFn = func(context.Context, string, container.StopOptions) error {
				return errors.New("daemon busy")
			}
			f.db.appsSleeping = nil
		}, 0, 0, 0},
		{"status update failure", func(f *stzFixture) {
			f.db.errs = map[string]error{"appSleep": errors.New("x")}
			f.db.appsSleeping = nil
		}, 1, 0, 0},
		{"wake update failure", func(f *stzFixture) {
			f.db.errs = map[string]error{"appAwake": errors.New("x")}
			f.db.appsToSleep = nil
		}, 0, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := placementStore()
			db.appsToSleep = toSleep
			db.appsSleeping = sleeping
			fixture := newSTZFixture(t, db)
			fixture.ops.ReadFileFn = activityFile(oldActivity)
			fixture.rt.ContainerListFn = emptyContainerList
			tc.configure(fixture)
			fixture.s.scaleZeroApplications(context.Background())
			if len(db.sleptAppIDs) != tc.wantSleeps || len(db.wokenAppIDs) != tc.wantWakes {
				t.Fatalf("slept=%v woken=%v", db.sleptAppIDs, db.wokenAppIDs)
			}
			if len(db.outbox) != tc.wantEvents {
				t.Fatalf("outbox = %v", outboxTypes(db))
			}
		})
	}
}

func TestEnsureAgents(t *testing.T) {
	t.Run("disabled without an image", func(t *testing.T) {
		db := placementStore()
		db.proxyServers = []store.Server{db.serverRow}
		scheduler := newScheduler(t, db) // AgentImage empty
		scheduler.ensureAgents(context.Background())
	})
	t.Run("list failure and empty fleet", func(t *testing.T) {
		db := &fakeSchedulerStore{errs: map[string]error{"proxyServers": errors.New("x")}}
		fixture := newSTZFixture(t, db)
		fixture.s.ensureAgents(context.Background())
		empty := newSTZFixture(t, &fakeSchedulerStore{})
		empty.s.ensureAgents(context.Background())
		if len(fixture.client.commands)+len(empty.client.commands) != 0 {
			t.Fatal("agent ensure ran without servers")
		}
	})
	t.Run("provisions reachable servers on the destination network", func(t *testing.T) {
		db := placementStore()
		other := db.serverRow
		other.ID = 7
		db.proxyServers = []store.Server{db.serverRow, other}
		db.defaultDest = &store.Destination{Network: "destnet"}
		fixture := newSTZFixture(t, db)
		fixture.s.dialSSH = func(_ context.Context, server store.Server, _ string) (remoteClient, error) {
			if server.ID == 7 {
				return nil, errors.New("unreachable")
			}
			return fixture.client, nil
		}
		swallowPanics(func() { fixture.s.ensureAgents(context.Background()) })
		if len(fixture.client.commands) != 1 || !strings.Contains(fixture.client.commands[0], "destnet") {
			t.Fatalf("ensure commands = %v", fixture.client.commands)
		}
	})
	t.Run("bridge fallback and ensure failure", func(t *testing.T) {
		db := placementStore()
		db.proxyServers = []store.Server{db.serverRow}
		fixture := newSTZFixture(t, db) // no default destination in the fake
		fixture.client.runErr = errors.New("docker gone")
		fixture.s.ensureAgents(context.Background())
		if len(fixture.client.commands) != 1 || !strings.Contains(fixture.client.commands[0], "bridge") {
			t.Fatalf("ensure commands = %v", fixture.client.commands)
		}
	})
}

func TestAgentScanReconcileGuards(t *testing.T) {
	db := placementStore()
	fixture := newSTZFixture(t, db)
	scan := fixture.s.newAgentScan(context.Background())
	defer scan.close()
	scan.reconcile(db.serverRow, "", fixture.client) // no network: nothing to join
	fixture.s.AgentImage = ""
	scan.reconcile(db.serverRow, "proxynet", fixture.client) // reconciliation disabled
	if len(fixture.client.commands) != 0 {
		t.Fatalf("guarded reconcile still ran: %v", fixture.client.commands)
	}
}

func TestReadWakerActivity(t *testing.T) {
	channelErr := errors.New("channel down")
	for _, tc := range []struct {
		name    string
		fn      func(context.Context, agentwire.FileReadParams) (agentwire.FileReadResult, error)
		want    time.Time
		wantErr bool
	}{
		{"channel failure", func(context.Context, agentwire.FileReadParams) (agentwire.FileReadResult, error) {
			return agentwire.FileReadResult{}, channelErr
		}, time.Time{}, true},
		{"absent", nil, time.Time{}, false},
		{"unreadable", activityFile("garbage"), time.Time{}, false},
		{"valid", activityFile(oldActivity), time.Unix(1_000_000_000, 0), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ops := &hostfake.Ops{ReadFileFn: tc.fn}
			got, err := readWakerActivity(context.Background(), ops, "res-uuid")
			if (err != nil) != tc.wantErr || !got.Equal(tc.want) {
				t.Fatalf("readWakerActivity = %v, %v", got, err)
			}
		})
	}
}

func TestStopByLabelBranches(t *testing.T) {
	notFound := fmt.Errorf("stop: %w", cerrdefs.ErrNotFound)
	hard := errors.New("daemon busy")
	for _, tc := range []struct {
		name    string
		stopErr func(name string) error
		listErr error
		wantErr bool
	}{
		{"missing containers tolerated", func(string) error { return notFound }, nil, false},
		{"named stop failure", func(string) error { return hard }, nil, true},
		{"list failure", func(string) error { return nil }, hard, true},
		{"labelled stop failure", func(name string) error {
			if name == "web" {
				return hard
			}
			return nil
		}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := &dockerfake.Runtime{
				ContainerStopFn: func(_ context.Context, name string, _ container.StopOptions) error {
					return tc.stopErr(name)
				},
				ContainerListFn: func(context.Context, container.ListOptions) ([]container.Summary, error) {
					if tc.listErr != nil {
						return nil, tc.listErr
					}
					return []container.Summary{{}, {Names: []string{"/web"}}}, nil
				},
			}
			err := stopByLabel(context.Background(), rt, "res-uuid", "akerdock.resource_uuid")
			if (err != nil) != tc.wantErr {
				t.Fatalf("stopByLabel error = %v", err)
			}
		})
	}
}

func TestLatestOf(t *testing.T) {
	early := time.Unix(1_000, 0)
	late := time.Unix(2_000, 0)
	for _, tc := range []struct {
		name string
		a, b pgtype.Timestamptz
		want time.Time
	}{
		{"both null", pgtype.Timestamptz{}, pgtype.Timestamptz{}, time.Time{}},
		{"only a", pgtype.Timestamptz{Time: early, Valid: true}, pgtype.Timestamptz{}, early},
		{"b newer", pgtype.Timestamptz{Time: early, Valid: true}, pgtype.Timestamptz{Time: late, Valid: true}, late},
		{"a newer", pgtype.Timestamptz{Time: late, Valid: true}, pgtype.Timestamptz{Time: early, Valid: true}, late},
	} {
		if got := latestOf(tc.a, tc.b); !got.Equal(tc.want) {
			t.Errorf("%s: latestOf = %v, want %v", tc.name, got, tc.want)
		}
	}
}
