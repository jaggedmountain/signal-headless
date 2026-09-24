// Package rpc is the daemon's client protocol: newline-delimited JSON-RPC 2.0
// over a unix socket, framed like signal-cli's jsonRpc mode.
package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const Version = "2.0"

type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// message is the union used when decoding traffic of unknown shape.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

type Notification struct {
	Method string
	Params json.RawMessage
}

type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

const (
	CodeParse          = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603
	CodeFailed         = -1 // application error
)

func Errorf(code int, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// --- server ---

// HandlerFunc serves one request. A returned *Error is sent as-is; other
// errors become CodeFailed.
type HandlerFunc func(ctx context.Context, c *Conn, method string, params json.RawMessage) (any, error)

type Server struct {
	ln      net.Listener
	path    string
	handler HandlerFunc
	onClose func(*Conn)

	mu    sync.Mutex
	conns map[*Conn]struct{}
}

// Listen binds the socket at path, replacing a stale socket file. It fails if
// another process is already serving there.
func Listen(path string, h HandlerFunc) (*Server, error) {
	if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
		c.Close()
		return nil, fmt.Errorf("another daemon is already listening on %s", path)
	}
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return &Server{ln: ln, path: path, handler: h, conns: map[*Conn]struct{}{}}, nil
}

// OnClose registers a callback run when a client disconnects.
func (s *Server) OnClose(fn func(*Conn)) { s.onClose = fn }

// Serve accepts clients until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		s.ln.Close()
	}()
	defer os.Remove(s.path)
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				s.closeAll()
				return nil
			}
			return err
		}
		c := newConn(nc)
		s.mu.Lock()
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		go func() {
			c.serve(ctx, s.handler)
			s.mu.Lock()
			delete(s.conns, c)
			s.mu.Unlock()
			if s.onClose != nil {
				s.onClose(c)
			}
		}()
	}
}

func (s *Server) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.conns {
		c.Close()
	}
}

// Each calls fn for every connected client.
func (s *Server) Each(fn func(*Conn)) {
	s.mu.Lock()
	conns := make([]*Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		fn(c)
	}
}

func (s *Server) NumConns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// Conn is one client connection. Writes go through a bounded queue so a slow
// client can never stall the daemon; overflowing it drops the client.
type Conn struct {
	nc         net.Conn
	out        chan []byte
	closed     chan struct{}
	finish     chan struct{}
	once       sync.Once
	finishOnce sync.Once

	// Native is set once the client subscribes to native events; until then
	// it receives signal-cli style "receive" notifications.
	Native atomic.Bool
	// Values lets the daemon attach per-connection state.
	Values sync.Map
}

const outQueue = 4096

func newConn(nc net.Conn) *Conn {
	c := &Conn{nc: nc, out: make(chan []byte, outQueue), closed: make(chan struct{}), finish: make(chan struct{})}
	go c.writer()
	return c
}

func (c *Conn) Close() {
	c.once.Do(func() {
		close(c.closed)
		c.nc.Close()
	})
}

func (c *Conn) writer() {
	w := bufio.NewWriter(c.nc)
	for {
		select {
		case <-c.closed:
			return
		case <-c.finish:
			// Client stopped sending: flush what's queued, then hang up.
			for {
				select {
				case b := <-c.out:
					if _, err := w.Write(b); err != nil {
						c.Close()
						return
					}
				default:
					_ = w.Flush()
					c.Close()
					return
				}
			}
		case b := <-c.out:
			c.nc.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if _, err := w.Write(b); err != nil {
				c.Close()
				return
			}
			// Flush when the queue drains to batch bursts.
			if len(c.out) == 0 {
				if err := w.Flush(); err != nil {
					c.Close()
					return
				}
			}
		}
	}
}

func (c *Conn) send(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	b = append(b, '\n')
	select {
	case <-c.closed:
	case c.out <- b:
	default:
		c.Close() // client isn't keeping up
	}
}

// Notify sends a JSON-RPC notification.
func (c *Conn) Notify(method string, params any) {
	c.send(struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{Version, method, params})
}

func (c *Conn) serve(ctx context.Context, h HandlerFunc) {
	var inflight sync.WaitGroup
	defer func() {
		inflight.Wait()
		c.finishOnce.Do(func() { close(c.finish) })
		<-c.closed
	}()
	sc := bufio.NewScanner(c.nc)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			c.send(Response{JSONRPC: Version, ID: json.RawMessage("null"), Error: Errorf(CodeParse, "parse error: %v", err)})
			continue
		}
		if req.Method == "" {
			c.send(Response{JSONRPC: Version, ID: nullID(req.ID), Error: Errorf(CodeInvalidRequest, "missing method")})
			continue
		}
		inflight.Add(1)
		go func(req Request) {
			defer inflight.Done()
			res, err := h(ctx, c, req.Method, req.Params)
			if len(req.ID) == 0 {
				return // notification from client: no response
			}
			resp := Response{JSONRPC: Version, ID: req.ID}
			if err != nil {
				var rerr *Error
				if errors.As(err, &rerr) {
					resp.Error = rerr
				} else {
					resp.Error = &Error{Code: CodeFailed, Message: err.Error()}
				}
			} else {
				b, merr := json.Marshal(res)
				if merr != nil {
					resp.Error = Errorf(CodeInternal, "marshal result: %v", merr)
				} else {
					resp.Result = b
				}
			}
			c.send(resp)
		}(req)
	}
}

func nullID(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

// --- client ---

type Client struct {
	nc      net.Conn
	wmu     sync.Mutex
	nextID  atomic.Int64
	mu      sync.Mutex
	pending map[string]chan Response
	notes   chan Notification
	done    chan struct{}
	err     error
}

// Dial connects to the daemon socket.
func Dial(path string) (*Client, error) {
	nc, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return nil, err
	}
	c := &Client{nc: nc, pending: map[string]chan Response{}, notes: make(chan Notification, 1024), done: make(chan struct{})}
	go c.reader()
	return c, nil
}

// Notifications delivers server notifications; closed when the connection ends.
func (c *Client) Notifications() <-chan Notification { return c.notes }

// Done is closed when the connection ends; Err then reports why.
func (c *Client) Done() <-chan struct{} { return c.done }
func (c *Client) Err() error             { return c.err }

func (c *Client) Close() error { return c.nc.Close() }

func (c *Client) reader() {
	defer close(c.done)
	defer close(c.notes)
	sc := bufio.NewScanner(c.nc)
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for sc.Scan() {
		var m message
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			continue
		}
		if m.Method != "" && len(m.ID) == 0 {
			c.notes <- Notification{Method: m.Method, Params: m.Params}
			continue
		}
		c.mu.Lock()
		ch := c.pending[string(m.ID)]
		delete(c.pending, string(m.ID))
		c.mu.Unlock()
		if ch != nil {
			ch <- Response{JSONRPC: m.JSONRPC, ID: m.ID, Result: m.Result, Error: m.Error}
		}
	}
	c.err = sc.Err()
	if c.err == nil {
		c.err = errors.New("daemon closed the connection")
	}
	c.mu.Lock()
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
}

// Call sends a request and decodes the result into out (may be nil).
func (c *Client) Call(ctx context.Context, method string, params, out any) error {
	id := strconv.FormatInt(c.nextID.Add(1), 10)
	idJSON := json.RawMessage(strconv.Quote(id))
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		raw = b
	}
	b, err := json.Marshal(Request{JSONRPC: Version, ID: idJSON, Method: method, Params: raw})
	if err != nil {
		return err
	}
	ch := make(chan Response, 1)
	c.mu.Lock()
	c.pending[string(idJSON)] = ch
	c.mu.Unlock()
	c.wmu.Lock()
	_, err = c.nc.Write(append(b, '\n'))
	c.wmu.Unlock()
	if err != nil {
		c.mu.Lock()
		delete(c.pending, string(idJSON))
		c.mu.Unlock()
		return err
	}
	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, string(idJSON))
		c.mu.Unlock()
		return ctx.Err()
	case resp, ok := <-ch:
		if !ok {
			return c.err
		}
		if resp.Error != nil {
			return resp.Error
		}
		if out != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, out)
		}
		return nil
	}
}
