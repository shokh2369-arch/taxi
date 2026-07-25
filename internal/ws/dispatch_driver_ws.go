package ws

import (
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"taxi-mvp/internal/auth"
	"taxi-mvp/internal/domain"
	"taxi-mvp/internal/safe"
)

type dispatchClient struct {
	hub  *DispatchHub
	conn *websocket.Conn
	send chan []byte
}

// ServeDriverDispatchWs upgrades to a driver-only websocket and streams dispatch poke events.
// Auth is performed on the upgrade request by Gin middleware (mirror query → headers, tryDriverID,
// RequireDriverAuth) — same rules as other driver routes.
//
// On connect, sends: { "type": "hello" }
// On change, sends: { "type": "dispatch_changed", "emitted_at": "<RFC3339 UTC>" }
func ServeDriverDispatchWs(hub *DispatchHub, w http.ResponseWriter, r *http.Request) {
	u := auth.UserFromContext(r.Context())
	if u == nil || u.Role != domain.RoleDriver {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"driver auth required"}`))
		return
	}

	conn, err := Upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ws: upgrade failed path=%s reason=%v", r.URL.Path, err)
		return
	}

	c := &dispatchClient{
		hub:  hub,
		conn: conn,
		send: make(chan []byte, 128),
	}

	if c.hub != nil {
		c.hub.register <- c
	}

	// Send hello immediately (best-effort).
	select {
	case c.send <- []byte(`{"type":"hello"}`):
	default:
	}

	safe.Go("ws_dispatch_write_pump", c.writePump)
	c.readPump()
}

func (c *dispatchClient) readPump() {
	defer func() {
		if c.hub != nil {
			c.hub.unregister <- c
		}
		c.conn.Close()
	}()
	c.conn.SetReadLimit(maxMessageSize)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})
	for {
		_, _, err := c.conn.ReadMessage()
		if err != nil {
			break
		}
	}
}

func (c *dispatchClient) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()
	for {
		select {
		case msg, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
