// The client half of ADR-067 §6: rendering the wake, and surviving a server
// that speaks frames this build does not know.
//
// Every top-level identifier is prefixed swake (concurrent-agent rule).
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/deepteams/akerdock/internal/terminal"
	tun "github.com/deepteams/akerdock/internal/tunnel"
)

// swakeSession builds a session whose control wire replays the given frames and
// then ends.
func swakeSession(t *testing.T, frames ...tun.HTTPControlFrame) (*egressSession, *bytes.Buffer) {
	t.Helper()
	var wire strings.Builder
	for _, frame := range frames {
		raw, err := json.Marshal(frame)
		if err != nil {
			t.Fatal(err)
		}
		wire.WriteString(string(raw) + "\n")
	}
	out := &bytes.Buffer{}
	control := tun.NewLineControl(strings.NewReader(wire.String()), out, nil, nil)
	return &egressSession{control: control}, out
}

// An unknown control-frame type must be dropped, not fatal: a server that gains
// a frame type would otherwise break every client older than it, which is what
// ADR-067's compatibility clause promises does not happen.
func TestSwakeUnknownControlFrameIsIgnored(t *testing.T) {
	session, _ := swakeSession(t,
		tun.HTTPControlFrame{Type: "something_from_the_future", Msg: "hello"},
		tun.HTTPControlFrame{Type: "session_close", Reason: "user_close"},
	)
	end, err := session.run(context.Background())
	if err != nil {
		t.Fatalf("an unknown frame broke the session: %v", err)
	}
	if end.reason != "user_close" {
		t.Fatalf("end reason = %q, want the close that followed the unknown frame", end.reason)
	}
}

// The cold-start notice widens the ONE budget it is about — the wait for the
// first connection to be served — and the ready notice puts it back.
func TestSwakeColdStartWidensTheFirstConnectionBudget(t *testing.T) {
	session := &egressSession{}
	if got := session.openTimeout(); got != egressDataOpenTimeout {
		t.Fatalf("budget before any wake = %s", got)
	}
	session.noteWaking(wakeFrameColdStart, "starting")
	if got := session.openTimeout(); got != egressWakeOpenTimeout {
		t.Fatalf("budget during a cold start = %s, want the wake ceiling", got)
	}
	if egressWakeOpenTimeout <= egressDataOpenTimeout {
		t.Fatal("the cold-start budget must outlast the ordinary one, or the two deadlines race")
	}
	session.noteWaking(wakeFrameReady, "ready")
	if got := session.openTimeout(); got != egressDataOpenTimeout {
		t.Fatalf("budget after the target came up = %s, want the ordinary one back", got)
	}
}

// The server's sentence is what reaches the developer: it names the target and
// the ceiling, and nothing on this side could reconstruct it.
func TestSwakeWakingFrameIsPrinted(t *testing.T) {
	session, _ := swakeSession(t,
		tun.HTTPControlFrame{Type: wakeControlFrame, Code: wakeFrameColdStart, Msg: "the target is asleep"},
		tun.HTTPControlFrame{Type: "session_close", Reason: "user_close"},
	)
	var runErr error
	_, stderr := captureOutput(t, func() {
		_, runErr = session.run(context.Background())
	})
	if runErr != nil {
		t.Fatal(runErr)
	}
	if !strings.Contains(stderr, "the target is asleep") {
		t.Fatalf("stderr = %q, want the server's own sentence", stderr)
	}
	if !session.waking.Load() {
		t.Fatal("the cold-start frame did not arm the wider budget")
	}
}

// The terminal has no data stream to answer on, so the same news travels as a
// text control message beside the end frame — and everything that is neither is
// still ignored.
func TestSwakeTerminalWakingNote(t *testing.T) {
	note, ok := swakeTerminalFrame(t, map[string]any{"type": "waking", "msg": "starting it"})
	if !ok || note != "starting it" {
		t.Fatalf("waking note = (%q, %v)", note, ok)
	}
	if _, ok := swakeTerminalFrame(t, map[string]any{"type": "waking"}); ok {
		t.Fatal("a waking frame with no sentence has nothing to print")
	}
	if _, ok := swakeTerminalFrame(t, map[string]any{"type": "resize", "cols": 80}); ok {
		t.Fatal("a resize frame was read as a wake notice")
	}
	if _, ok := swakeTerminalFrame(t, map[string]any{"type": "end", "reason": "wake_failed"}); ok {
		t.Fatal("an end frame was read as a wake notice")
	}
	if _, ok := terminalWakingNote([]byte("not json")); ok {
		t.Fatal("garbage was read as a wake notice")
	}
	// And the end frame keeps carrying the verdict, which is the value the CLI
	// already renders.
	end, ok := terminalEndReason(swakeJSON(t, map[string]any{
		"type": "end", "reason": "wake_failed", "msg": "stalled on shop-postgres",
	}))
	if !ok || end.reason != "wake_failed" || end.message != "stalled on shop-postgres" {
		t.Fatalf("end frame = %+v (%v)", end, ok)
	}
	if got := terminalEndMessage(end.reason, end.message); !strings.Contains(got, "stalled on shop-postgres") {
		t.Fatalf("the developer reads %q, not the waker's own sentence", got)
	}
}

// The mint's `state` and the control frame are one event on two channels, and
// both exist because either can be missing. A client that read the state must
// not print the frame's repeat of it — two lines read as two wakes.
func TestSwakeColdStartIsAnnouncedOnceForTheTunnel(t *testing.T) {
	t.Run("announced by the mint, the frame is silent", func(t *testing.T) {
		session := &egressSession{}
		session.waking.Store(true)
		session.announced.Store(true)
		_, stderr := captureOutput(t, func() {
			session.noteWaking(wakeFrameColdStart, wakeColdStartNotice)
		})
		if stderr != "" {
			t.Fatalf("the cold start was announced twice: %q", stderr)
		}
		// The budget is still the wide one: suppressing the LINE must not
		// suppress the fact.
		if got := session.openTimeout(); got != egressWakeOpenTimeout {
			t.Fatalf("budget = %s, want the wake ceiling", got)
		}
	})

	t.Run("not announced by the mint, the frame speaks", func(t *testing.T) {
		session := &egressSession{}
		_, stderr := captureOutput(t, func() {
			session.noteWaking(wakeFrameColdStart, wakeColdStartNotice)
		})
		if !strings.Contains(stderr, "scale-to-zero") {
			t.Fatalf("stderr = %q, want the cold-start notice", stderr)
		}
		// And a repeat of the frame is still one line.
		_, again := captureOutput(t, func() {
			session.noteWaking(wakeFrameColdStart, wakeColdStartNotice)
		})
		if again != "" {
			t.Fatalf("a repeated frame printed again: %q", again)
		}
	})

	t.Run("the ready notice is a different event and always speaks", func(t *testing.T) {
		session := &egressSession{}
		session.announced.Store(true)
		_, stderr := captureOutput(t, func() {
			session.noteWaking(wakeFrameReady, "the target is ready")
		})
		if !strings.Contains(stderr, "ready") {
			t.Fatalf("stderr = %q — the target coming up is news of its own", stderr)
		}
	})
}

// Same rule on the terminal, where the notice matters more: the alternative is
// a blank window held for up to 75 s.
func TestSwakeColdStartIsAnnouncedOnceForTheTerminal(t *testing.T) {
	installShellStdin(t)
	run := func(t *testing.T, announced bool) string {
		t.Helper()
		conn := &fakeTerminalConn{in: make(chan fakeTerminalMessage, 4)}
		conn.in <- fakeTerminalMessage{
			typ:  terminal.MessageText,
			data: []byte(`{"type":"waking","msg":"` + wakeColdStartNotice + `"}`),
		}
		conn.in <- fakeTerminalMessage{
			typ:  terminal.MessageText,
			data: []byte(`{"type":"end","reason":"user_close"}`),
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, stderr := captureOutput(t, func() { _ = runTerminalPumps(ctx, conn, announced) })
		return stderr
	}

	if got := run(t, true); strings.Contains(got, "scale-to-zero") {
		t.Fatalf("the cold start was announced twice: %q", got)
	}
	if got := run(t, false); !strings.Contains(got, "scale-to-zero") {
		t.Fatalf("stderr = %q, want the cold-start notice from the frame", got)
	}
}

// The whole point of the mint carrying the state is WHEN the developer reads
// it: before anything is opened, not after the listener is announced and they
// are already typing into what looks like a hung tunnel.
func TestSwakeMintStateIsAnnouncedBeforeAnythingIsOpened(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"message":"the server is not reachable over SSH right now"}`))
	}))
	defer srv.Close()
	client := &Client{base: srv.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var err error
	_, stderr := captureOutput(t, func() {
		err = client.forwardSession(ctx, mintedTunnel{
			attachPath: "/tunnel/attach", token: "tk", waking: true,
		}, 0, 5432)
	})
	if err == nil {
		t.Fatal("the refused attach must still fail")
	}
	notice := strings.Index(stderr, wakeColdStartNotice)
	if notice < 0 {
		t.Fatalf("stderr = %q, want the cold-start notice from the mint state", stderr)
	}
	// Nothing was listening yet when it was printed: the attach was refused, so
	// the listener line never came at all — and the notice still reached the
	// developer, which is the ordering this field exists for.
	if strings.Contains(stderr, "forwarding 127.0.0.1") {
		t.Fatalf("a refused attach announced a listener: %q", stderr)
	}

	// And a ready mint says nothing.
	_, quiet := captureOutput(t, func() {
		_ = client.forwardSession(ctx, mintedTunnel{attachPath: "/tunnel/attach", token: "tk"}, 0, 5432)
	})
	if strings.Contains(quiet, wakeColdStartNotice) {
		t.Fatalf("a ready mint announced a cold start: %q", quiet)
	}
}

// An absent state is `ready`, and it has to be for two unrelated reasons at
// once: the target was up, or the manager predates the field. Neither prints a
// notice and neither widens a budget.
func TestSwakeAbsentMintStateIsReady(t *testing.T) {
	for _, state := range []string{"", "ready", "something_from_the_future"} {
		if got := state == mintStateWaking; got {
			t.Fatalf("state %q read as waking", state)
		}
	}
	if mintStateWaking != "waking" {
		t.Fatalf("mintStateWaking = %q — it must match the contract's enum", mintStateWaking)
	}
	session := &egressSession{}
	if session.waking.Load() || session.announced.Load() {
		t.Fatal("a session minted with no state must start ready and unannounced")
	}
	if got := session.openTimeout(); got != egressDataOpenTimeout {
		t.Fatalf("budget = %s, want the ordinary one", got)
	}
}

func swakeTerminalFrame(t *testing.T, payload map[string]any) (string, bool) {
	t.Helper()
	return terminalWakingNote(swakeJSON(t, payload))
}

func swakeJSON(t *testing.T, payload map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
