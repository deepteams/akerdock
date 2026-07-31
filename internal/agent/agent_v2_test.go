package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/docker/docker/api/types/container"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/dockerruntime/fake"
)

// TestAgentExecutesCommandsOverV2 proves the ADR-052 rail end to end on the
// agent side: the agent offers v2, the control plane picks it, sends a typed
// command over the same connection observations ride on, and the executor's
// answer comes back with the daemon's data — while the observation batch
// still gets its ack.
func TestAgentExecutesCommandsOverV2(t *testing.T) {
	rt := &fake.Runtime{}
	rt.ContainerInspectFn = func(_ context.Context, name string) (container.InspectResponse, error) {
		return container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{ID: "id-" + name}}, nil
	}

	results := make(chan agentwire.Result, 4)
	sawObservations := make(chan int64, 4)
	mux := http.NewServeMux()
	mux.HandleFunc("/agent/v1/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			Subprotocols: []string{agentwire.SubprotocolV2, agentwire.SubprotocolV1},
		})
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		if conn.Subprotocol() != agentwire.SubprotocolV2 {
			return // the agent must have offered v2: it has an executor
		}
		ctx := r.Context()
		write := func(f agentwire.Frame) error {
			data, err := json.Marshal(f)
			if err != nil {
				return err
			}
			return conn.Write(ctx, websocket.MessageText, data)
		}
		params, _ := json.Marshal(agentwire.NameParams{Name: "akerdock-app"})
		if write(agentwire.Frame{Type: agentwire.FrameCommand, Cmd: &agentwire.Command{
			ID: 42, Method: agentwire.MethodContainerInspect, Params: params,
		}}) != nil {
			return
		}
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var f agentwire.Frame
			if json.Unmarshal(data, &f) != nil {
				continue
			}
			switch f.Type {
			case agentwire.FrameObservations:
				sawObservations <- f.Seq
				if write(agentwire.Frame{Type: agentwire.FrameAck, Seq: f.Seq}) != nil {
					return
				}
			case agentwire.FrameResult:
				if f.Res != nil {
					results <- *f.Res
				}
			}
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := NewAgent(Enrollment{InstanceURL: srv.URL, Token: "akda_test"}, nil, nil)
	a.Executor = NewExecutor(rt, nil, nil)
	a.Flush = 10 * time.Millisecond
	a.Backoff = 5 * time.Millisecond
	a.Heartbeat = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx) // the immediate hello heartbeat opens the channel

	select {
	case res := <-results:
		if res.ID != 42 || res.Err != nil {
			t.Fatalf("result = %+v", res)
		}
		var resp container.InspectResponse
		if err := json.Unmarshal(res.Body, &resp); err != nil {
			t.Fatal(err)
		}
		if resp.ID != "id-akerdock-app" {
			t.Fatalf("inspect over the channel = %+v", resp)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no command result on the v2 channel")
	}

	select {
	case <-sawObservations:
		// The hello heartbeat rode the same connection and was acked through
		// the read loop — observations and commands share the rail.
	case <-time.After(3 * time.Second):
		t.Fatal("observations stopped flowing on the v2 channel")
	}
}
