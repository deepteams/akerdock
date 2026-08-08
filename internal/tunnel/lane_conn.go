package tunnel

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"sync"
)

// MultiLaneConn stripes the WebSocket v1 wire over a bounded set of physical
// WebSockets. Session control stays on lane zero; stream-specific control
// follows that stream's data lane. The first outgoing frame pins a stream to
// one available lane, preserving per-stream ordering even when a secondary
// lane joins after the session started.
type MultiLaneConn struct {
	ctx    context.Context
	cancel context.CancelFunc
	max    int

	mu          sync.Mutex
	lanes       map[int]Conn
	closeLane   map[int]func() error
	laneWrites  map[int]*sync.Mutex
	streamLanes map[uint32]int
	closed      bool

	incoming  chan multiLaneFrame
	done      chan struct{}
	doneOnce  sync.Once
	closeOnce sync.Once
	closeErr  error
}

type multiLaneFrame struct {
	typ  MessageType
	data []byte
	err  error
}

// NewMultiLaneConn starts with the authenticated primary lane. max includes
// that primary lane and is normally four for ingress fallback.
func NewMultiLaneConn(primary Conn, closePrimary func() error, laneLimit int) *MultiLaneConn {
	if laneLimit < 1 {
		laneLimit = 1
	}
	if closePrimary == nil {
		closePrimary = func() error { return nil }
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &MultiLaneConn{
		ctx: ctx, cancel: cancel, max: laneLimit,
		lanes: map[int]Conn{0: primary}, closeLane: map[int]func() error{0: closePrimary},
		laneWrites:  map[int]*sync.Mutex{0: {}},
		streamLanes: make(map[uint32]int), incoming: make(chan multiLaneFrame, laneLimit), done: make(chan struct{}),
	}
	go c.readLane(primary)
	return c
}

// AddLane authenticates at the HTTP handler before it reaches this method.
func (c *MultiLaneConn) AddLane(index int, lane Conn, closeLane func() error) error {
	if index < 1 || index >= c.max {
		return errors.New("tunnel: WebSocket lane index is out of range")
	}
	if closeLane == nil {
		closeLane = func() error { return nil }
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrOriginClosed
	}
	if _, exists := c.lanes[index]; exists {
		c.mu.Unlock()
		return errors.New("tunnel: WebSocket lane is already attached")
	}
	c.lanes[index] = lane
	c.closeLane[index] = closeLane
	c.laneWrites[index] = &sync.Mutex{}
	c.mu.Unlock()
	go c.readLane(lane)
	return nil
}

func (c *MultiLaneConn) readLane(lane Conn) {
	for {
		typ, data, err := lane.Read(c.ctx)
		frame := multiLaneFrame{typ: typ, data: data, err: err}
		select {
		case c.incoming <- frame:
		case <-c.ctx.Done():
			return
		}
		if err != nil {
			c.doneOnce.Do(func() { close(c.done) })
			return
		}
	}
}

func (c *MultiLaneConn) Read(ctx context.Context) (MessageType, []byte, error) {
	select {
	case frame := <-c.incoming:
		return frame.typ, frame.data, frame.err
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	case <-c.ctx.Done():
		return 0, nil, ErrOriginClosed
	}
}

func (c *MultiLaneConn) Write(ctx context.Context, typ MessageType, data []byte) error {
	var streamID uint32
	if typ == MessageBinary && len(data) >= 4 {
		streamID = binary.BigEndian.Uint32(data[:4])
		laneIndex, lane := c.streamLane(streamID)
		return c.writeLane(ctx, laneIndex, lane, typ, data)
	}

	if typ == MessageText {
		var control ctrl
		if json.Unmarshal(data, &control) == nil && control.ID != 0 && control.T != "open" {
			// Stream-specific controls travel on the same physical socket as
			// their data. In particular, eof must never overtake the last data
			// frame merely because lane zero was less congested.
			laneIndex, lane := c.streamLane(control.ID)
			err := c.writeLane(ctx, laneIndex, lane, typ, data)
			if control.T == "eof" || control.T == "close" || control.T == "open_err" {
				c.mu.Lock()
				delete(c.streamLanes, control.ID)
				c.mu.Unlock()
			}
			return err
		}
	}
	c.mu.Lock()
	primary := c.lanes[0]
	c.mu.Unlock()
	return c.writeLane(ctx, 0, primary, typ, data)
}

func (c *MultiLaneConn) streamLane(streamID uint32) (int, Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	laneIndex, exists := c.streamLanes[streamID]
	if !exists {
		preferred := int(streamID % uint32(c.max))
		if _, available := c.lanes[preferred]; !available {
			preferred = 0
		}
		laneIndex = preferred
		c.streamLanes[streamID] = laneIndex
	}
	return laneIndex, c.lanes[laneIndex]
}

func (c *MultiLaneConn) writeLane(ctx context.Context, index int, lane Conn, typ MessageType, data []byte) error {
	c.mu.Lock()
	writeMu := c.laneWrites[index]
	c.mu.Unlock()
	writeMu.Lock()
	defer writeMu.Unlock()
	return lane.Write(ctx, typ, data)
}

// parallelWrites marks that MultiLaneConn serializes writes per physical lane
// itself, so the mux need not retain the legacy session-wide writer lock.
func (*MultiLaneConn) parallelWrites() {}

// Ping checks every currently attached physical lane.
func (c *MultiLaneConn) Ping(ctx context.Context) error {
	c.mu.Lock()
	lanes := make([]Conn, 0, len(c.lanes))
	for i := 0; i < c.max; i++ {
		if lane := c.lanes[i]; lane != nil {
			lanes = append(lanes, lane)
		}
	}
	c.mu.Unlock()
	for _, lane := range lanes {
		if err := lane.Ping(ctx); err != nil {
			return err
		}
	}
	return nil
}

// Done closes when any physical lane drops or Close tears down the group.
func (c *MultiLaneConn) Done() <-chan struct{} { return c.done }

// LaneCount reports the currently attached physical sockets.
func (c *MultiLaneConn) LaneCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.lanes)
}

// Close tears every lane down once.
func (c *MultiLaneConn) Close() error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		closers := make([]func() error, 0, len(c.closeLane))
		for _, closeLane := range c.closeLane {
			closers = append(closers, closeLane)
		}
		c.mu.Unlock()
		c.cancel()
		c.doneOnce.Do(func() { close(c.done) })
		for _, closeLane := range closers {
			if err := closeLane(); c.closeErr == nil && err != nil {
				c.closeErr = err
			}
		}
	})
	return c.closeErr
}
