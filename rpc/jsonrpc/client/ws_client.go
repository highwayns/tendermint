package client

import (
	"context"
	"encoding/json"
	"fmt"
	mrand "math/rand"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	metrics "github.com/rcrowley/go-metrics"

	tmsync "github.com/tendermint/tendermint/internal/libs/sync"
	tmclient "github.com/tendermint/tendermint/rpc/client"
	rpctypes "github.com/tendermint/tendermint/rpc/jsonrpc/types"
)

// WSOptions for WSClient.
type WSOptions struct {
	MaxReconnectAttempts uint          // maximum attempts to reconnect
	ReadWait             time.Duration // deadline for any read op
	WriteWait            time.Duration // deadline for any write op
	PingPeriod           time.Duration // frequency with which pings are sent
	SkipMetrics          bool          // do not keep metrics for ping/pong latency
}

// DefaultWSOptions returns default WS options.
func DefaultWSOptions() WSOptions {
	return WSOptions{
		MaxReconnectAttempts: 10, // first: 2 sec, last: 17 min.
		WriteWait:            10 * time.Second,
		ReadWait:             0,
		PingPeriod:           0,
	}
}

// WSClient is a JSON-RPC client, which uses WebSocket for communication with
// the remote server.
//
// WSClient is safe for concurrent use by multiple goroutines.
type WSClient struct { // nolint: maligned
	*tmclient.RunState
	conn *websocket.Conn

	Address  string // IP:PORT or /path/to/socket
	Endpoint string // /websocket/url/endpoint
	Dialer   func(string, string) (net.Conn, error)

	// Single user facing channel to read RPCResponses from, closed only when the
	// client is being stopped.
	ResponsesCh chan rpctypes.RPCResponse

	// Callback, which will be called each time after successful reconnect.
	onReconnect func()

	// internal channels
	send            chan rpctypes.RPCRequest // user requests
	backlog         chan rpctypes.RPCRequest // stores a single user request received during a conn failure
	reconnectAfter  chan error               // reconnect requests
	readRoutineQuit chan struct{}            // a way for readRoutine to close writeRoutine

	// Maximum reconnect attempts (0 or greater; default: 25).
	maxReconnectAttempts uint

	// Support both ws and wss protocols
	protocol string

	wg sync.WaitGroup

	mtx            tmsync.RWMutex
	sentLastPingAt time.Time
	reconnecting   bool
	nextReqID      int
	// sentIDs        map[types.JSONRPCIntID]bool // IDs of the requests currently in flight

	// Time allowed to write a message to the server. 0 means block until operation succeeds.
	writeWait time.Duration

	// Time allowed to read the next message from the server. 0 means block until operation succeeds.
	readWait time.Duration

	// Send pings to server with this period. Must be less than readWait. If 0, no pings will be sent.
	pingPeriod time.Duration

	// Time between sending a ping and receiving a pong. See
	// https://godoc.org/github.com/rcrowley/go-metrics#Timer.
	PingPongLatencyTimer metrics.Timer
}

// NewWS returns a new client. The endpoint argument must begin with a `/`. An
// error is returned on invalid remote.
// It uses DefaultWSOptions.
func NewWS(remoteAddr, endpoint string) (*WSClient, error) {
	return NewWSWithOptions(remoteAddr, endpoint, DefaultWSOptions())
}

// NewWSWithOptions allows you to provide custom WSOptions.
func NewWSWithOptions(remoteAddr, endpoint string, opts WSOptions) (*WSClient, error) {
	parsedURL, err := newParsedURL(remoteAddr)
	if err != nil {
		return nil, err
	}
	// default to ws protocol, unless wss is explicitly specified
	if parsedURL.Scheme != protoWSS {
		parsedURL.Scheme = protoWS
	}

	dialFn, err := makeHTTPDialer(remoteAddr)
	if err != nil {
		return nil, err
	}

	c := &WSClient{
		RunState:             tmclient.NewRunState("WSClient", nil),
		Address:              parsedURL.GetTrimmedHostWithPath(),
		Dialer:               dialFn,
		Endpoint:             endpoint,
		maxReconnectAttempts: opts.MaxReconnectAttempts,
		readWait:             opts.ReadWait,
		writeWait:            opts.WriteWait,
		pingPeriod:           opts.PingPeriod,
		protocol:             parsedURL.Scheme,

		// sentIDs: make(map[types.JSONRPCIntID]bool),
	}

	switch opts.SkipMetrics {
	case true:
		c.PingPongLatencyTimer = metrics.NilTimer{}
	case false:
		c.PingPongLatencyTimer = metrics.NewTimer()
	}

	return c, nil
}

// OnReconnect sets the callback, which will be called every time after
// successful reconnect.
// Could only be set before Start.
func (c *WSClient) OnReconnect(cb func()) {
	c.onReconnect = cb
}

// String returns WS client full address.
func (c *WSClient) String() string {
	return fmt.Sprintf("WSClient{%s (%s)}", c.Address, c.Endpoint)
}

// Start dials the specified service address and starts the I/O routines.
func (c *WSClient) Start(ctx context.Context) error {
	if err := c.RunState.Start(ctx); err != nil {
		return err
	}
	err := c.dial()
	if err != nil {
		return err
	}

	c.ResponsesCh = make(chan rpctypes.RPCResponse)

	c.send = make(chan rpctypes.RPCRequest)
	// 1 additional error may come from the read/write
	// goroutine depending on which failed first.
	c.reconnectAfter = make(chan error, 1)
	// capacity for 1 request. a user won't be able to send more because the send
	// channel is unbuffered.
	c.backlog = make(chan rpctypes.RPCRequest, 1)

	c.startReadWriteRoutines(ctx)
	go c.reconnectRoutine(ctx)

	return nil
}

// Stop shuts down the client.
func (c *WSClient) Stop() error {
	if err := c.RunState.Stop(); err != nil {
		return err
	}

	// only close user-facing channels when we can't write to them
	c.wg.Wait()
	close(c.ResponsesCh)

	return nil
}

// IsReconnecting returns true if the client is reconnecting right now.
func (c *WSClient) IsReconnecting() bool {
	c.mtx.RLock()
	defer c.mtx.RUnlock()
	return c.reconnecting
}

// IsActive returns true if the client is running and not reconnecting.
func (c *WSClient) IsActive() bool {
	return c.IsRunning() && !c.IsReconnecting()
}

// Send the given RPC request to the server. Results will be available on
// ResponsesCh, errors, if any, on ErrorsCh. Will block until send succeeds or
// ctx.Done is closed.
func (c *WSClient) Send(ctx context.Context, request rpctypes.RPCRequest) error {
	select {
	case c.send <- request:
		c.Logger.Info("sent a request", "req", request)
		// c.mtx.Lock()
		// c.sentIDs[request.ID.(types.JSONRPCIntID)] = true
		// c.mtx.Unlock()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Call enqueues a call request onto the Send queue. Requests are JSON encoded.
func (c *WSClient) Call(ctx context.Context, method string, params map[string]interface{}) error {
	request, err := rpctypes.MapToRequest(c.nextRequestID(), method, params)
	if err != nil {
		return err
	}
	return c.Send(ctx, request)
}

// CallWithArrayParams enqueues a call request onto the Send queue. Params are
// in a form of array (e.g. []interface{}{"abcd"}). Requests are JSON encoded.
func (c *WSClient) CallWithArrayParams(ctx context.Context, method string, params []interface{}) error {
	request, err := rpctypes.ArrayToRequest(c.nextRequestID(), method, params)
	if err != nil {
		return err
	}
	return c.Send(ctx, request)
}

// Private methods

func (c *WSClient) nextRequestID() rpctypes.JSONRPCIntID {
	c.mtx.Lock()
	id := c.nextReqID
	c.nextReqID++
	c.mtx.Unlock()
	return rpctypes.JSONRPCIntID(id)
}

func (c *WSClient) dial() error {
	dialer := &websocket.Dialer{
		NetDial: c.Dialer,
		Proxy:   http.ProxyFromEnvironment,
	}
	rHeader := http.Header{}
	conn, _, err := dialer.Dial(c.protocol+"://"+c.Address+c.Endpoint, rHeader) // nolint:bodyclose
	if err != nil {
		return err
	}
	c.conn = conn
	return nil
}

// reconnect tries to redial up to maxReconnectAttempts with exponential
// backoff.
func (c *WSClient) reconnect(ctx context.Context) error {
	attempt := uint(0)

	c.mtx.Lock()
	c.reconnecting = true
	c.mtx.Unlock()
	defer func() {
		c.mtx.Lock()
		c.reconnecting = false
		c.mtx.Unlock()
	}()

	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		// nolint:gosec // G404: Use of weak random number generator
		jitter := time.Duration(mrand.Float64() * float64(time.Second)) // 1s == (1e9 ns)
		backoffDuration := jitter + ((1 << attempt) * time.Second)

		c.Logger.Info("reconnecting", "attempt", attempt+1, "backoff_duration", backoffDuration)
		timer.Reset(backoffDuration)
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}

		err := c.dial()
		if err != nil {
			c.Logger.Error("failed to redial", "err", err)
		} else {
			c.Logger.Info("reconnected")
			if c.onReconnect != nil {
				go c.onReconnect()
			}
			return nil
		}

		attempt++

		if attempt > c.maxReconnectAttempts {
			return fmt.Errorf("reached maximum reconnect attempts: %w", err)
		}
	}
}

func (c *WSClient) startReadWriteRoutines(ctx context.Context) {
	c.wg.Add(2)
	c.readRoutineQuit = make(chan struct{})
	go c.readRoutine(ctx)
	go c.writeRoutine(ctx)
}

func (c *WSClient) processBacklog() error {
	select {
	case request := <-c.backlog:
		if c.writeWait > 0 {
			if err := c.conn.SetWriteDeadline(time.Now().Add(c.writeWait)); err != nil {
				c.Logger.Error("failed to set write deadline", "err", err)
			}
		}
		if err := c.conn.WriteJSON(request); err != nil {
			c.Logger.Error("failed to resend request", "err", err)
			c.reconnectAfter <- err
			// requeue request
			c.backlog <- request
			return err
		}
		c.Logger.Info("resend a request", "req", request)
	default:
	}
	return nil
}

func (c *WSClient) reconnectRoutine(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case originalError := <-c.reconnectAfter:
			// wait until writeRoutine and readRoutine finish
			c.wg.Wait()
			if err := c.reconnect(ctx); err != nil {
				c.Logger.Error("failed to reconnect", "err", err, "original_err", originalError)
				if err = c.Stop(); err != nil {
					c.Logger.Error("failed to stop conn", "error", err)
				}

				return
			}
			// drain reconnectAfter
		LOOP:
			for {
				select {
				case <-ctx.Done():
					return
				case <-c.reconnectAfter:
				default:
					break LOOP
				}
			}
			err := c.processBacklog()
			if err == nil {
				c.startReadWriteRoutines(ctx)
			}
		}
	}
}

// The client ensures that there is at most one writer to a connection by
// executing all writes from this goroutine.
func (c *WSClient) writeRoutine(ctx context.Context) {
	var ticker *time.Ticker
	if c.pingPeriod > 0 {
		// ticker with a predefined period
		ticker = time.NewTicker(c.pingPeriod)
	} else {
		// ticker that never fires
		ticker = &time.Ticker{C: make(<-chan time.Time)}
	}

	defer func() {
		ticker.Stop()
		c.conn.Close()
		// err != nil {
		// ignore error; it will trigger in tests
		// likely because it's closing an already closed connection
		// }
		c.wg.Done()
	}()

	for {
		select {
		case request := <-c.send:
			if c.writeWait > 0 {
				if err := c.conn.SetWriteDeadline(time.Now().Add(c.writeWait)); err != nil {
					c.Logger.Error("failed to set write deadline", "err", err)
				}
			}
			if err := c.conn.WriteJSON(request); err != nil {
				c.Logger.Error("failed to send request", "err", err)
				c.reconnectAfter <- err
				// add request to the backlog, so we don't lose it
				c.backlog <- request
				return
			}
		case <-ticker.C:
			if c.writeWait > 0 {
				if err := c.conn.SetWriteDeadline(time.Now().Add(c.writeWait)); err != nil {
					c.Logger.Error("failed to set write deadline", "err", err)
				}
			}
			if err := c.conn.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
				c.Logger.Error("failed to write ping", "err", err)
				c.reconnectAfter <- err
				return
			}
			c.mtx.Lock()
			c.sentLastPingAt = time.Now()
			c.mtx.Unlock()
			c.Logger.Debug("sent ping")
		case <-c.readRoutineQuit:
			return
		case <-ctx.Done():
			if err := c.conn.WriteMessage(
				websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			); err != nil {
				c.Logger.Error("failed to write message", "err", err)
			}
			return
		}
	}
}

// The client ensures that there is at most one reader to a connection by
// executing all reads from this goroutine.
func (c *WSClient) readRoutine(ctx context.Context) {
	defer func() {
		c.conn.Close()
		// err != nil {
		// ignore error; it will trigger in tests
		// likely because it's closing an already closed connection
		// }
		c.wg.Done()
	}()

	c.conn.SetPongHandler(func(string) error {
		// gather latency stats
		c.mtx.RLock()
		t := c.sentLastPingAt
		c.mtx.RUnlock()
		c.PingPongLatencyTimer.UpdateSince(t)

		c.Logger.Debug("got pong")
		return nil
	})

	for {
		// reset deadline for every message type (control or data)
		if c.readWait > 0 {
			if err := c.conn.SetReadDeadline(time.Now().Add(c.readWait)); err != nil {
				c.Logger.Error("failed to set read deadline", "err", err)
			}
		}
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			if !websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure) {
				return
			}

			c.Logger.Error("failed to read response", "err", err)
			close(c.readRoutineQuit)
			c.reconnectAfter <- err
			return
		}

		var response rpctypes.RPCResponse
		err = json.Unmarshal(data, &response)
		if err != nil {
			c.Logger.Error("failed to parse response", "err", err, "data", string(data))
			continue
		}

		if err = validateResponseID(response.ID); err != nil {
			c.Logger.Error("error in response ID", "id", response.ID, "err", err)
			continue
		}

		// TODO: events resulting from /subscribe do not work with ->
		// because they are implemented as responses with the subscribe request's
		// ID. According to the spec, they should be notifications (requests
		// without IDs).
		// https://github.com/tendermint/tendermint/issues/2949
		// c.mtx.Lock()
		// if _, ok := c.sentIDs[response.ID.(types.JSONRPCIntID)]; !ok {
		// 	c.Logger.Error("unsolicited response ID", "id", response.ID, "expected", c.sentIDs)
		// 	c.mtx.Unlock()
		// 	continue
		// }
		// delete(c.sentIDs, response.ID.(types.JSONRPCIntID))
		// c.mtx.Unlock()
		// Combine a non-blocking read on BaseService.Quit with a non-blocking write on ResponsesCh to avoid blocking
		// c.wg.Wait() in c.Stop(). Note we rely on Quit being closed so that it sends unlimited Quit signals to stop
		// both readRoutine and writeRoutine

		c.Logger.Info("got response", "id", response.ID, "result", response.Result)

		select {
		case <-ctx.Done():
			return
		case c.ResponsesCh <- response:
		}
	}
}

// Predefined methods

// Subscribe to a query. Note the server must have a "subscribe" route
// defined.
func (c *WSClient) Subscribe(ctx context.Context, query string) error {
	params := map[string]interface{}{"query": query}
	return c.Call(ctx, "subscribe", params)
}

// Unsubscribe from a query. Note the server must have a "unsubscribe" route
// defined.
func (c *WSClient) Unsubscribe(ctx context.Context, query string) error {
	params := map[string]interface{}{"query": query}
	return c.Call(ctx, "unsubscribe", params)
}

// UnsubscribeAll from all. Note the server must have a "unsubscribe_all" route
// defined.
func (c *WSClient) UnsubscribeAll(ctx context.Context) error {
	params := map[string]interface{}{}
	return c.Call(ctx, "unsubscribe_all", params)
}
