// Package waker implements the scale-to-zero helper (ADR-036, proxy-contract
// §8): a reverse-proxy run as a mode of the AkerDock binary, in front of every
// scale_to_zero resource on a server. It wakes the target container on the
// first request (docker start, await healthy, hold-and-forward) and dates
// activity into a per-resource file the control plane reads over SSH.
//
// Docker and the filesystem are behind interfaces so the wake decision, the
// limits (§8.3) and the activity accounting are unit-testable without a daemon.
package waker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

// errWakeTimeout marks a genuine wake deadline overrun (→ 504), as opposed to an
// operational failure like an unreachable Docker socket or a missing container
// (→ 502). Both used to surface as "wake timed out", which hid the real cause.
var errWakeTimeout = errors.New("wake timed out")

// ContainerState is the subset of `docker inspect` the waker needs.
type ContainerState struct {
	Running bool
	// Health is the healthcheck status: "healthy", "unhealthy", "starting", or
	// "none" when the image declares no healthcheck (§8.2, running-stable path).
	Health string
}

// Docker is the container control the waker is code-limited to (§8.1): it only
// starts, inspects and stops akerdock.managed containers — never
// create/remove/build. Stop exists solely to roll back a failed wake: it is
// the same operation the control plane's sleep performs.
type Docker interface {
	Start(ctx context.Context, container string) error
	Inspect(ctx context.Context, container string) (ContainerState, error)
	Stop(ctx context.Context, container string) error
}

// Activity records the last request time of a resource so the control plane's
// sleep pass can read it over SSH (ADR-036 §2).
type Activity interface {
	Record(uuid string, at time.Time) error
}

// Route maps one public host to the container that serves it. Several routes
// may share a ResourceUUID (a compose preview with one host per service).
type Route struct {
	Host         string `json:"host"`
	ResourceUUID string `json:"resource_uuid"`
	Container    string `json:"container"`
	Port         int    `json:"port"`
}

// Resource is the wake unit: the containers started together (a whole compose
// stack, or a single container for a plain app), listed in the stack's
// topological start order (§2.6) — the waker wakes them in this order, each
// one ready before the next starts, exactly like the deploy did.
type Resource struct {
	UUID       string   `json:"uuid"`
	Containers []string `json:"containers"`
}

// Config is the routing table the control plane deposits for the waker
// (/var/lib/akerdock/waker/routes.json). The waker never generates it.
type Config struct {
	Routes    []Route    `json:"routes"`
	Resources []Resource `json:"resources"`
}

// DefaultListenAddr is the port the waker listens on. It MUST match
// proxy.WakerPort — the dynamic file routes scale-to-zero traffic to
// http://akerdock-waker:8080 (ADR-036 §2).
const DefaultListenAddr = ":8080"

// UptimeProbeHeader marks an AkerDock uptime check (ADR-037): the waker wakes
// and forwards it (so the check measures the app truly up), but does NOT record
// it as activity — otherwise monitoring would keep a scale-to-zero app awake
// forever. The app wakes briefly per check, then sleeps again.
const UptimeProbeHeader = "X-AkerDock-Uptime"

const (
	// MaxHoldBody is the largest request body held across a cold start (§8.3):
	// beyond it the waker returns 503 rather than buffer a big upload while the
	// target boots.
	MaxHoldBody = 1 << 20 // 1 MiB
	// defaultWakeTimeout bounds a wake before it becomes a 504 (§8.3).
	defaultWakeTimeout = 60 * time.Second
	defaultPoll        = 500 * time.Millisecond
	// defaultStableFor is how long a container with no healthcheck must stay
	// running before it counts as awake (§8.2, running-stable-10s).
	defaultStableFor = 10 * time.Second
)

// Waker is the http.Handler front of the scale-to-zero resources.
type Waker struct {
	docker   Docker
	activity Activity
	now      func() time.Time

	WakeTimeout time.Duration
	Poll        time.Duration
	StableFor   time.Duration
	// Logger records wake failures with their real cause; nil uses slog.Default.
	Logger *slog.Logger

	// newProxy builds the reverse proxy to a running target; overridable in tests.
	newProxy func(target *url.URL) http.Handler

	mu        sync.Mutex
	byHost    map[string]Route
	resources map[string]Resource
	// gate serialises the wake of one resource so N concurrent first-requests
	// trigger a single docker start (single-flight).
	gate map[string]*sync.Mutex
	// wakes tracks the asynchronous wakes behind the waiting page (§8.2): a
	// browser navigation is answered immediately with the page, and the wake
	// runs in a background goroutine whose outcome is rendered on the next
	// auto-refresh. A config reload rebuilds the Waker and loses this state;
	// the worst case is a duplicate wake attempt, which docker start absorbs
	// (idempotent) and the per-resource gate serialises.
	wakes map[string]*wakeState
}

// wakeState is the lifecycle of one asynchronous wake: in progress, or failed
// with its cause until a retry clears it.
type wakeState struct {
	waking bool
	err    error
}

// New builds a Waker from a routing config. now defaults to time.Now.
func New(cfg Config, docker Docker, activity Activity, now func() time.Time) *Waker {
	if now == nil {
		now = time.Now
	}
	w := &Waker{
		docker:      docker,
		activity:    activity,
		now:         now,
		WakeTimeout: defaultWakeTimeout,
		Poll:        defaultPoll,
		StableFor:   defaultStableFor,
		byHost:      make(map[string]Route, len(cfg.Routes)),
		resources:   make(map[string]Resource, len(cfg.Resources)),
		gate:        make(map[string]*sync.Mutex),
		wakes:       make(map[string]*wakeState),
	}
	for _, r := range cfg.Routes {
		w.byHost[r.Host] = r
	}
	for _, res := range cfg.Resources {
		w.resources[res.UUID] = res
		w.gate[res.UUID] = &sync.Mutex{}
	}
	w.newProxy = func(target *url.URL) http.Handler {
		return httputil.NewSingleHostReverseProxy(target)
	}
	return w
}

func hostname(h string) string {
	host, _, _ := strings.Cut(h, ":")
	return host
}

func (w *Waker) log() *slog.Logger {
	if w.Logger != nil {
		return w.Logger
	}
	return slog.Default()
}

func (w *Waker) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	route, ok := w.byHost[hostname(req.Host)]
	if !ok {
		http.Error(rw, "unknown host", http.StatusNotFound)
		return
	}

	// An uptime probe (ADR-037) must never wake a deliberately-sleeping app just
	// to be answered — that would cold-start the whole stack on every check. When
	// the target is asleep, the waker replies directly "up" (it IS available: it
	// wakes on real traffic); when already awake, the probe is forwarded to the
	// real app below so the check measures its true health.
	isUptime := req.Header.Get(UptimeProbeHeader) != ""
	if isUptime && !w.isRunning(req.Context(), route.ResourceUUID) {
		rw.Header().Set("X-AkerDock-Scale", "asleep")
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte("asleep"))
		return
	}

	// A body larger than the hold budget is refused during a cold start rather
	// than buffered while the target boots (§8.3).
	if req.ContentLength > MaxHoldBody && !w.isRunning(req.Context(), route.ResourceUUID) {
		rw.Header().Set("Retry-After", "5")
		http.Error(rw, "resource waking, body too large to hold", http.StatusServiceUnavailable)
		return
	}

	// A browser navigation on a sleeping resource gets the waiting page (§8.2)
	// instead of a connection held through the whole cold start: the wake runs
	// in the background and the page auto-refreshes with each container's
	// state until the stack answers for itself. Only safe navigations qualify —
	// holding stays correct for API clients and for bodies a page cannot replay.
	if isBrowserNavigation(req) && !w.isRunning(req.Context(), route.ResourceUUID) {
		w.serveWaitingPage(rw, req, route)
		return
	}

	if err := w.ensureAwake(req.Context(), route.ResourceUUID); err != nil {
		w.log().Warn("waker: wake failed", "host", hostname(req.Host),
			"resource", route.ResourceUUID, "container", route.Container, "error", err)
		if errors.Is(err, errWakeTimeout) {
			http.Error(rw, "wake timed out", http.StatusGatewayTimeout)
		} else {
			// Operational failure (Docker socket unreachable, container missing,
			// API version…): report it instead of masking it as a timeout.
			http.Error(rw, "wake failed: "+err.Error(), http.StatusBadGateway)
		}
		return
	}

	// Activity is dated on every request — that is what "the waker reports
	// activity" means (ADR-036): the control plane reads this to decide sleep.
	// An uptime probe of an already-awake app is forwarded (real health) but must
	// not count as activity, or monitoring would keep the app awake forever.
	if w.activity != nil && !isUptime {
		_ = w.activity.Record(route.ResourceUUID, w.now())
	}

	target := &url.URL{Scheme: "http", Host: fmt.Sprintf("%s:%d", route.Container, route.Port)}
	stripWakeParams(req)
	w.newProxy(target).ServeHTTP(rw, req)
}

// retryParam is the query flag of the waiting page's retry link: it clears a
// failed wake and starts a new attempt. Stripped before proxying so the app
// never sees it.
const retryParam = "akd_wake_retry"

// isBrowserNavigation reports whether the request is a page navigation a human
// is watching: a safe method with an HTML Accept. Anything else — API calls,
// form posts, uploads — keeps the hold-and-forward path, whose reply the
// client can actually consume.
func isBrowserNavigation(req *http.Request) bool {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return false
	}
	return strings.Contains(req.Header.Get("Accept"), "text/html")
}

// stripWakeParams removes the waiting page's own query flags before the
// request reaches the application.
func stripWakeParams(req *http.Request) {
	if !strings.Contains(req.URL.RawQuery, retryParam) {
		return
	}
	q := req.URL.Query()
	q.Del(retryParam)
	req.URL.RawQuery = q.Encode()
}

// serveWaitingPage answers a browser navigation on a sleeping resource: it
// kicks the wake off in the background (single-flight) and renders each
// container's state, auto-refreshing until the stack is up — at which point
// the refresh is proxied like any request. A failed wake renders its cause
// and stops refreshing; the retry link starts a fresh attempt.
func (w *Waker) serveWaitingPage(rw http.ResponseWriter, req *http.Request, route Route) {
	uuid := route.ResourceUUID
	retry := req.URL.Query().Has(retryParam)

	w.mu.Lock()
	ws := w.wakes[uuid]
	if ws == nil {
		ws = &wakeState{}
		w.wakes[uuid] = ws
	}
	failed := ws.err
	if !ws.waking && (failed == nil || retry) {
		ws.waking, ws.err = true, nil
		failed = nil
		go w.wakeInBackground(uuid, ws)
	}
	w.mu.Unlock()

	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.Header().Set("Cache-Control", "no-store")
	rw.Header().Set("X-AkerDock-Scale", "waking")
	if failed == nil {
		rw.Header().Set("Retry-After", "2")
	}
	rw.WriteHeader(http.StatusServiceUnavailable)
	if req.Method == http.MethodHead {
		return
	}
	_, _ = rw.Write([]byte(w.renderWaitingPage(req.Context(), hostname(req.Host), uuid, failed)))
}

// wakeInBackground runs one wake attempt detached from the request that
// triggered it — the browser already got its page. Success records the
// activity of that original navigation; failure is kept for the page to show.
func (w *Waker) wakeInBackground(uuid string, ws *wakeState) {
	ctx, cancel := context.WithTimeout(context.Background(), w.WakeTimeout+30*time.Second)
	defer cancel()
	err := w.ensureAwake(ctx, uuid)
	if err != nil {
		w.log().Warn("waker: background wake failed", "resource", uuid, "error", err)
	} else if w.activity != nil {
		_ = w.activity.Record(uuid, w.now())
	}
	w.mu.Lock()
	ws.waking, ws.err = false, err
	w.mu.Unlock()
}

// containerDisplayState is the waiting page's view of one container of the
// wake set, in start order.
type containerDisplayState struct {
	Name  string // service name, resource prefix trimmed
	State string // waiting | starting | running | ready | unhealthy
}

// wakeDisplayStates snapshots the wake set for the page. Inspect errors render
// as "waiting" rather than failing the page — the wake goroutine will surface
// a real error if there is one.
func (w *Waker) wakeDisplayStates(ctx context.Context, uuid string) []containerDisplayState {
	res, ok := w.resource(uuid)
	if !ok {
		return nil
	}
	out := make([]containerDisplayState, 0, len(res.Containers))
	for _, c := range res.Containers {
		name := strings.TrimPrefix(c, uuid+"-")
		if name == uuid || name == "" {
			name = "app"
		}
		state := "waiting"
		if st, err := w.docker.Inspect(ctx, c); err == nil {
			switch {
			case !st.Running:
				state = "waiting"
			case st.Health == "healthy":
				state = "ready"
			case st.Health == "starting":
				state = "starting"
			case st.Health == "unhealthy":
				state = "unhealthy"
			default: // no healthcheck: running is all we can observe
				state = "running"
			}
		}
		out = append(out, containerDisplayState{Name: name, State: state})
	}
	return out
}

// renderWaitingPage builds the self-contained HTML: no external assets (the
// stack serving them is asleep), auto-refresh while waking, a retry link on
// failure. English, like every UI string (§25.2).
func (w *Waker) renderWaitingPage(ctx context.Context, host, uuid string, failed error) string {
	var b strings.Builder
	b.WriteString("<!doctype html><html><head><meta charset=\"utf-8\">")
	b.WriteString("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">")
	if failed == nil {
		b.WriteString("<meta http-equiv=\"refresh\" content=\"2\">")
	}
	fmt.Fprintf(&b, "<title>Waking up — %s</title>", htmlEscape(host))
	b.WriteString("<style>")
	b.WriteString("body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;" +
		"background:#101014;color:#e6e6ea;font:15px/1.5 system-ui,sans-serif}" +
		"main{max-width:26rem;padding:2rem}" +
		"h1{font-size:1.15rem;font-weight:600;margin:0 0 .35rem}" +
		"p{margin:.25rem 0;color:#9a9aa5}" +
		"ul{list-style:none;margin:1.25rem 0 0;padding:0}" +
		"li{display:flex;align-items:center;gap:.6rem;padding:.3rem 0;font-family:ui-monospace,monospace;font-size:.85rem}" +
		".dot{width:.55rem;height:.55rem;border-radius:50%;flex:none}" +
		".waiting .dot{background:#3a3a44}" +
		".starting .dot,.running .dot{background:#d9a441;animation:p 1.2s ease-in-out infinite}" +
		".ready .dot{background:#4cb782}" +
		".unhealthy .dot{background:#d9534f}" +
		".state{margin-left:auto;color:#9a9aa5}" +
		".err{margin-top:1rem;padding:.75rem 1rem;border:1px solid #5a2e2e;border-radius:.5rem;background:#1d1416;color:#e0a5a2;font-size:.85rem;word-break:break-word}" +
		"a{color:#7aa2f7}" +
		"@keyframes p{50%{opacity:.35}}")
	b.WriteString("</style></head><body><main>")
	if failed == nil {
		fmt.Fprintf(&b, "<h1>Waking up %s…</h1>", htmlEscape(host))
		b.WriteString("<p>This application was asleep. Its services are starting; the page refreshes by itself.</p>")
	} else {
		fmt.Fprintf(&b, "<h1>%s could not wake up</h1>", htmlEscape(host))
		b.WriteString("<p>The application was put back to sleep.</p>")
	}
	b.WriteString("<ul>")
	for _, c := range w.wakeDisplayStates(ctx, uuid) {
		fmt.Fprintf(&b, "<li class=\"%s\"><span class=\"dot\"></span>%s<span class=\"state\">%s</span></li>",
			c.State, htmlEscape(c.Name), c.State)
	}
	b.WriteString("</ul>")
	if failed != nil {
		fmt.Fprintf(&b, "<div class=\"err\">%s</div>", htmlEscape(failed.Error()))
		fmt.Fprintf(&b, "<p><a href=\"?%s=1\">Try again</a></p>", retryParam)
	}
	b.WriteString("</main></body></html>")
	return b.String()
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

// isRunning reports whether every container of the resource is already running,
// i.e. no wake is needed (cheap fast-path check).
func (w *Waker) isRunning(ctx context.Context, uuid string) bool {
	res, ok := w.resource(uuid)
	if !ok {
		return false
	}
	for _, c := range res.Containers {
		st, err := w.docker.Inspect(ctx, c)
		if err != nil || !st.Running {
			return false
		}
	}
	return true
}

func (w *Waker) resource(uuid string) (Resource, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	res, ok := w.resources[uuid]
	return res, ok
}

func (w *Waker) gateFor(uuid string) *sync.Mutex {
	w.mu.Lock()
	defer w.mu.Unlock()
	g, ok := w.gate[uuid]
	if !ok {
		g = &sync.Mutex{}
		w.gate[uuid] = g
	}
	return g
}

// ensureAwake wakes the resource's containers in their listed order — the
// stack's compose start order — waiting for each to be ready before starting
// the next, so a dependency (database, broker) is up and resolvable before the
// service that needs it boots. Blocks until all are ready or the wake budget
// runs out. Serialised per resource so concurrent first-requests share one
// wake. A failed wake stops the containers this attempt started, so the
// resource returns to its slept state instead of crash-looping half-awake
// while the control plane still believes it asleep.
func (w *Waker) ensureAwake(ctx context.Context, uuid string) error {
	res, ok := w.resource(uuid)
	if !ok {
		return fmt.Errorf("waker: unknown resource %s", uuid)
	}

	g := w.gateFor(uuid)
	g.Lock()
	defer g.Unlock()

	deadline := w.now().Add(w.WakeTimeout)
	firstRunning := make(map[string]time.Time)
	startedByWake := map[string]bool{}
	var started []string

	err := func() error {
		for _, c := range res.Containers {
			for {
				st, err := w.docker.Inspect(ctx, c)
				if err != nil {
					return err
				}
				if st.Running && w.ready(c, st, firstRunning) {
					break // awake — move on to the next of the order
				}
				if !st.Running {
					// Idempotent: starting an already-running container is harmless.
					if err := w.docker.Start(ctx, c); err != nil {
						return err
					}
					if !startedByWake[c] {
						startedByWake[c] = true
						started = append(started, c)
					}
					delete(firstRunning, c)
				}
				if !w.now().Before(deadline) {
					return fmt.Errorf("%w: %s after %s", errWakeTimeout, uuid, w.WakeTimeout)
				}
				if err := sleepCtx(ctx, w.Poll); err != nil {
					return err
				}
			}
		}
		return nil
	}()
	if err != nil {
		w.rollback(uuid, started)
	}
	return err
}

// rollback stops the containers a failed wake started, in reverse order —
// best-effort, on a fresh context because the request's is likely dead. This
// also disarms the compose restart policy: a `docker stop` marks the container
// deliberately stopped, ending any crash-loop the partial wake caused.
func (w *Waker) rollback(uuid string, started []string) {
	if len(started) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for i := len(started) - 1; i >= 0; i-- {
		if err := w.docker.Stop(ctx, started[i]); err != nil {
			w.log().Warn("waker: rollback stop failed",
				"resource", uuid, "container", started[i], "error", err)
		}
	}
}

// ready decides whether a running container counts as awake: healthy if it has a
// healthcheck, else running-stable for StableFor (§8.2). starting/unhealthy are
// not ready.
func (w *Waker) ready(container string, st ContainerState, firstRunning map[string]time.Time) bool {
	switch st.Health {
	case "healthy":
		return true
	case "unhealthy", "starting":
		delete(firstRunning, container)
		return false
	default: // "none" or empty: no healthcheck declared → running-stable window.
		if _, seen := firstRunning[container]; !seen {
			firstRunning[container] = w.now()
		}
		return w.now().Sub(firstRunning[container]) >= w.StableFor
	}
}

// sleepCtx sleeps for d, or returns early if ctx is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
