// Command wsprobe drives a terminal WebSocket from the E2E suite (§24.4,
// ADR-024). It exists because the suite speaks curl, and curl does not speak
// WebSocket — and because a shell script cannot assert on a PTY: the point of
// the test is that keystrokes reach a real terminal and its output comes back.
//
// Usage:
//
//	wsprobe -url <ws url> [-send <keystrokes>] [-expect <substring>]
//	        [-timeout 20s] [-close user|drop] [-idle]
//
// Exit status is 0 when everything asserted held. The end reason announced by
// the server is printed on stdout as "end: <reason>", so the caller can assert
// on it (idle_timeout, user_close…).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
)

func main() {
	url := flag.String("url", "", "WebSocket URL, token included")
	send := flag.String("send", "", "keystrokes to type (\\n is appended)")
	expect := flag.String("expect", "", "substring the terminal must print")
	timeout := flag.Duration("timeout", 20*time.Second, "overall deadline")
	closeMode := flag.String("close", "user", "how to end: user (clean close) or drop (kill the socket)")
	idle := flag.Bool("idle", false, "type nothing and wait for the server to end the session")
	flag.Parse()

	if *url == "" {
		fail("-url is required")
	}
	// -idle is the whole point of the idle-timeout assertion: type nothing,
	// and let the SERVER end the session. Typing anything would reset the
	// very timer under test.
	if *idle && (*send != "" || *expect != "") {
		fail("-idle types nothing: it cannot be combined with -send or -expect")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, *url, nil)
	if err != nil {
		fail("dial: %v", err)
	}
	// A terminal streams; the default read limit would truncate a chatty shell.
	conn.SetReadLimit(1 << 20)
	defer func() { _ = conn.CloseNow() }()

	if *send != "" {
		// Give the shell a moment to print its prompt: typing into a pty that
		// has not started echoes nothing.
		time.Sleep(500 * time.Millisecond)
		if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":120,"rows":40}`)); err != nil {
			fail("resize: %v", err)
		}
		if err := conn.Write(ctx, websocket.MessageBinary, []byte(*send+"\n")); err != nil {
			fail("write: %v", err)
		}
	}

	var output strings.Builder
	endReason := ""
	seen := false   // the expected output has been printed
	ending := false // the session has been asked to end
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			// The server closing after announcing its end reason is a normal
			// finish; anything else with nothing announced is a failure.
			if endReason != "" || websocket.CloseStatus(err) == websocket.StatusNormalClosure {
				break
			}
			if ctx.Err() != nil {
				fail("timed out after %s; output so far:\n%s", *timeout, output.String())
			}
			fail("read: %v; output so far:\n%s", err, output.String())
		}

		if typ == websocket.MessageText {
			if reason, ok := parseEnd(data); ok {
				endReason = reason
			}
			continue
		}
		output.Write(data)

		if *expect == "" || ending || !strings.Contains(output.String(), *expect) {
			continue
		}
		seen = true

		// End the session, then KEEP READING: the server announces why it
		// ended in a text frame, and a probe that stopped at its own last
		// keystroke would never see it.
		ending = true
		switch *closeMode {
		case "user":
			// Ending the shell is how a terminal is really closed. It also
			// exercises the path where the PTY reaches EOF on its own.
			if err := conn.Write(ctx, websocket.MessageBinary, []byte("exit\n")); err != nil {
				fail("exit: %v", err)
			}
		case "drop":
			// A yanked cable, not a goodbye: the server must notice by itself
			// and kill the pty. Nothing more will arrive here.
			_ = conn.CloseNow()
			goto done
		default:
			fail("unknown -close mode %q", *closeMode)
		}
	}

done:
	if *expect != "" && !seen {
		fail("terminal never printed %q; got:\n%s", *expect, output.String())
	}

	if endReason != "" {
		fmt.Printf("end: %s\n", endReason)
	}
	fmt.Print(output.String())
}

// parseEnd reads the server's {"type":"end","reason":"…"} frame without
// pulling in a JSON dependency for two fields.
func parseEnd(data []byte) (string, bool) {
	s := string(data)
	if !strings.Contains(s, `"type":"end"`) {
		return "", false
	}
	_, rest, ok := strings.Cut(s, `"reason":"`)
	if !ok {
		return "", false
	}
	reason, _, ok := strings.Cut(rest, `"`)
	if !ok {
		return "", false
	}
	return reason, true
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "wsprobe: "+format+"\n", args...)
	os.Exit(1)
}
