package browsersession

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/coder/websocket"
)

// errBrowserGone reports that the debugger connection died, usually because
// the browser window was closed.
var errBrowserGone = errors.New("el navegador cerró la conexión de depuración")

// cdpError is a DevTools protocol refusal.
type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// cdpMessage is one protocol frame: a reply (id set) or an event.
type cdpMessage struct {
	ID     int             `json:"id"`
	Error  *cdpError       `json:"error"`
	Result json.RawMessage `json:"result"`
}

// cdp is a minimal DevTools client: commands are correlated by id and events
// are discarded, since the session only polls.
type cdp struct {
	conn    *websocket.Conn
	mu      sync.Mutex
	next    int
	pending map[int]chan cdpMessage
	closed  chan struct{}
}

// dialCDP connects to a browser's DevTools websocket.
func dialCDP(ctx context.Context, wsURL string) (*cdp, error) {
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("no se pudo conectar con el depurador del navegador: %w", err)
	}
	c := &cdp{conn: conn, pending: make(map[int]chan cdpMessage), closed: make(chan struct{})}
	go c.read()
	return c, nil
}

// read pumps frames until the connection dies.
func (c *cdp) read() {
	defer close(c.closed)
	for {
		_, data, err := c.conn.Read(context.Background())
		if err != nil {
			return
		}
		var msg cdpMessage
		if json.Unmarshal(data, &msg) != nil || msg.ID == 0 {
			continue
		}
		c.mu.Lock()
		reply := c.pending[msg.ID]
		delete(c.pending, msg.ID)
		c.mu.Unlock()
		if reply != nil {
			reply <- msg
		}
	}
}

// call sends one command and waits for its reply. session routes it to a
// page's devtools session; empty means the browser itself.
func (c *cdp) call(ctx context.Context, session, method string, params any, out any) error {
	c.mu.Lock()
	c.next++
	id := c.next
	reply := make(chan cdpMessage, 1)
	c.pending[id] = reply
	c.mu.Unlock()

	frame := map[string]any{"id": id, "method": method}
	if params != nil {
		frame["params"] = params
	}
	if session != "" {
		frame["sessionId"] = session
	}
	data, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	if err := c.conn.Write(ctx, websocket.MessageText, data); err != nil {
		return fmt.Errorf("no se pudo hablar con el navegador: %w", err)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return errBrowserGone
	case msg := <-reply:
		if msg.Error != nil {
			return fmt.Errorf("%s: %s", method, msg.Error.Message)
		}
		if out != nil && len(msg.Result) > 0 {
			return json.Unmarshal(msg.Result, out)
		}
		return nil
	}
}

// close ends the debugger connection; the browser keeps running.
func (c *cdp) close() {
	_ = c.conn.Close(websocket.StatusNormalClosure, "")
}

// quit asks the browser to close itself.
func (c *cdp) quit(ctx context.Context) {
	_ = c.call(ctx, "", "Browser.close", nil, nil)
}
