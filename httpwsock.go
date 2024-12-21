package httpwsockgo

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// debug message
type WSM struct {
	WSM_TYPE string `json:"WSM_TYPE"`
	WSM_DATA string `json:"WSM_DATA"`
}

type WSM_DATA_LOG struct {
	Log string `json:"log"`
}

type WSM_LOG struct {
	WSM_TYPE string       `json:"WSM_TYPE"`
	WSM_DATA WSM_DATA_LOG `json:"WSM_DATA"`
}

// Hub maintains the set of active clients and broadcasts messages to the
// clients.
type Hub struct {
	Mu sync.RWMutex
	// Registered clients.
	Clients map[*Client]bool

	// Inbound messages from the clients.
	Broadcast chan []byte

	/*Msg        chan MsgChangeGPIO
	MsgRequest chan MsgRequestData*/

	// Register requests from the clients.
	Register chan *Client

	// Unregister requests from clients.
	Unregister chan *Client
}

func NewHub() *Hub {
	return &Hub{
		Mu:         sync.RWMutex{},
		Broadcast:  make(chan []byte),
		Register:   make(chan *Client),
		Unregister: make(chan *Client),
		Clients:    make(map[*Client]bool),
	}
}

const (
	// Time allowed to write a message to the peer.
	writeWait = 10 * time.Second

	// Time allowed to read the next pong message from the peer.
	pongWait = 60 * time.Second

	// Send pings to peer with this period. Must be less than pongWait.
	pingPeriod = (pongWait * 9) / 10

	// Maximum message size allowed from peer.
	maxMessageSize = 512
)

var (
	newline = []byte{'\n'}
	space   = []byte{' '}
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// Client is a middleman between the websocket connection and the hub.
type Client struct {
	GET_ARG url.Values
	//
	hub *Hub
	// The websocket connection.
	conn *websocket.Conn
	// Buffered channel of outbound messages.
	Send chan []byte
}

// readPump pumps messages from the websocket connection to the hub.
//
// The application runs readPump in a per-connection goroutine. The application
// ensures that there is at most one reader on a connection by executing all
// reads from this goroutine.
func (c *Client) readPump() {
	defer func() {
		c.hub.Unregister <- c
		c.conn.Close()
	}()
	c.conn.SetReadLimit(maxMessageSize)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error { c.conn.SetReadDeadline(time.Now().Add(pongWait)); return nil })
	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("error: %v", err)
			}
			break
		}
		message = bytes.TrimSpace(bytes.Replace(message, newline, space, -1))
		c.hub.Broadcast <- message
	}
}

// writePump pumps messages from the hub to the websocket connection.
//
// A goroutine running writePump is started for each connection. The
// application ensures that there is at most one writer to a connection by
// executing all writes from this goroutine.
func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()
	for {
		select {
		case message, ok := <-c.Send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				// The hub closed the channel.
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			w, err := c.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			w.Write(message)
			// Add queued chat messages to the current websocket message.
			n := len(c.Send)
			for i := 0; i < n; i++ {
				w, err = c.conn.NextWriter(websocket.TextMessage)
				if err != nil {
					return
				}
				w.Write(<-c.Send)
			}

			if err := w.Close(); err != nil {
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

func RequestsWebSocket(hub *Hub, w http.ResponseWriter, r *http.Request) {
	fmt.Printf("RequestsWs:%s\n", r.URL.Path)
	/*	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}*/

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}
	client := &Client{hub: hub, conn: conn, Send: make(chan []byte, 256), GET_ARG: r.URL.Query()}

	client.hub.Register <- client

	// Allow collection of memory referenced by the caller by doing all work in
	// new goroutines.
	go client.writePump()
	go client.readPump()
}

func (h *Hub) Run(wg *sync.WaitGroup, ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			// log.Println("Hub run Done")
			return
		case client := <-h.Register:
			log.Printf("Register WSClient")
			h.Mu.Lock()
			h.Clients[client] = true
			h.Mu.Unlock()
		case client := <-h.Unregister:
			log.Printf("UnRegister WSClient")
			h.Mu.Lock()
			if _, ok := h.Clients[client]; ok {
				delete(h.Clients, client)
				close(client.Send)
			}
			h.Mu.Unlock()
		case <-time.After(time.Millisecond * 1000):
			if len(h.Clients) > 0 {
			}
		}
	}
}

func (h *Hub) SendBroadCastMsg(message []byte) {
	h.Mu.RLock()
	defer h.Mu.RUnlock()

	for client := range h.Clients {
		select {
		case client.Send <- message:
		default:
			h.Unregister <- client
		}
	}
}

func (h *Hub) Close(message []byte) {
	h.Mu.Lock()
	defer h.Mu.Unlock()

	for client := range h.Clients {
		client.conn.Close()
	}
}
