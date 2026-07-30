package collab

// Package collab implements the real-time collaboration hub: WebSocket-based
// presence/cursor broadcast and per-document concurrency control with a
// degradation strategy (PRD F1.3: 单文档并发上限与降级策略).
//
// Beyond the concurrency cap, when the limit is exceeded new joiners are
// admitted read-only and notified (一人编辑、他人只读+提示). The hub is the
// seam where a full Yjs CRDT server (yjs-server, Node.js) would plug in;
// here we implement the Go-side presence/cursor relay and admission control
// that the Wiki API owns.

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wiki/wiki-backend/internal/domain"
)

// Presence describes a collaborator's live state on a document.
type Presence struct {
	UserID   domain.UUID `json:"user_id"`
	Name     string      `json:"name"`
	Color    string      `json:"color"`
	Cursor   *Cursor     `json:"cursor,omitempty"`
	ReadOnly bool        `json:"read_only"`
	JoinedAt time.Time   `json:"joined_at"`
}

type Cursor struct {
	BlockID string `json:"block_id"`
	Offset  int    `json:"offset"`
}

// Message is the envelope exchanged over the collaboration WebSocket.
type Message struct {
	Type     string          `json:"type"` // join|leave|presence|cursor|update|degraded|denied
	From     domain.UUID     `json:"from,omitempty"`
	Payload  json.RawMessage `json:"payload,omitempty"`
}

// Client is a connected collaborator.
type Client struct {
	UserID   domain.UUID
	Name     string
	DocID    domain.UUID
	ReadOnly bool
	send     chan Message
}

// Hub manages presence and admission per document.
type Hub struct {
	mu       sync.RWMutex
	rooms    map[domain.UUID]*room
	maxConc  int
}

type room struct {
	mu        sync.RWMutex
	clients   map[domain.UUID]*Client
	editors   int // count of non-read-only clients
}

func NewHub(maxConcurrent int) *Hub {
	if maxConcurrent < 1 {
		maxConcurrent = 50
	}
	return &Hub{rooms: map[domain.UUID]*room{}, maxConc: maxConcurrent}
}

// JoinResult reports the admission outcome for a join attempt.
type JoinResult struct {
	Admitted  bool
	ReadOnly  bool
	Reason    string
}

// Register adds a client to a document room. If the editor count is at the
// limit, the client is admitted read-only (degradation). Returns the current
// presence roster on success.
func (h *Hub) Register(c *Client) (JoinResult, []Presence) {
	h.mu.Lock()
	r, ok := h.rooms[c.DocID]
	if !ok {
		r = &room{clients: map[domain.UUID]*Client{}}
		h.rooms[c.DocID] = r
	}
	h.mu.Unlock()

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.clients[c.UserID]; exists {
		// already in room; refresh
		return JoinResult{Admitted: true, ReadOnly: c.ReadOnly}, roster(r)
	}

	degraded := false
	if !c.ReadOnly && r.editors >= h.maxConc {
		// degrade: admit as read-only
		c.ReadOnly = true
		degraded = true
	}
	r.clients[c.UserID] = c
	if !c.ReadOnly {
		r.editors++
	}
	c.send = make(chan Message, 32)

	res := JoinResult{Admitted: true, ReadOnly: c.ReadOnly}
	if degraded {
		res.Reason = "document at concurrency limit; admitted read-only"
	}
	return res, roster(r)
}

// Unregister removes a client and broadcasts leave.
func (h *Hub) Unregister(docID, userID domain.UUID) {
	h.mu.RLock()
	r, ok := h.rooms[docID]
	h.mu.RUnlock()
	if !ok {
		return
	}
	r.mu.Lock()
	c, exists := r.clients[userID]
	if exists {
		delete(r.clients, userID)
		if !c.ReadOnly {
			r.editors--
		}
	}
	empty := len(r.clients) == 0
	r.mu.Unlock()

	if empty {
		h.mu.Lock()
		if cur, ok := h.rooms[docID]; ok && len(cur.clients) == 0 {
			delete(h.rooms, docID)
		}
		h.mu.Unlock()
	}
}

// Broadcast sends a message to all clients in a document room.
func (h *Hub) Broadcast(docID domain.UUID, msg Message) {
	h.mu.RLock()
	r, ok := h.rooms[docID]
	h.mu.RUnlock()
	if !ok {
		return
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, c := range r.clients {
		select {
		case c.send <- msg:
		default: // drop if client buffer full
		}
	}
}

// UpdateCursor updates a collaborator's cursor and broadcasts presence.
func (h *Hub) UpdateCursor(docID, userID domain.UUID, cur Cursor) {
	h.mu.RLock()
	r, ok := h.rooms[docID]
	h.mu.RUnlock()
	if !ok {
		return
	}
	r.mu.RLock()
	c, exists := r.clients[userID]
	r.mu.RUnlock()
	if !exists {
		return
	}
	_ = c // presence derived from roster; cursor carried in broadcast payload
	payload, _ := json.Marshal(Presence{UserID: userID, Cursor: &cur})
	h.Broadcast(docID, Message{Type: "cursor", From: userID, Payload: payload})
}

// Roster returns the presence list for a document.
func (h *Hub) Roster(docID domain.UUID) []Presence {
	h.mu.RLock()
	r, ok := h.rooms[docID]
	h.mu.RUnlock()
	if !ok {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return roster(r)
}

func roster(r *room) []Presence {
	out := make([]Presence, 0, len(r.clients))
	for _, c := range r.clients {
		out = append(out, Presence{
			UserID: c.UserID, Name: c.Name, ReadOnly: c.ReadOnly, JoinedAt: time.Now(),
		})
	}
	return out
}

// Concurrency returns the configured max concurrent editors.
func (h *Hub) Concurrency() int { return h.maxConc }

// EditorCount returns the active editor count for a document (test helper).
func (h *Hub) EditorCount(docID domain.UUID) int {
	h.mu.RLock()
	r, ok := h.rooms[docID]
	h.mu.RUnlock()
	if !ok {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.editors
}

// Send returns a client's send channel (for the WS writer loop).
func (c *Client) Send() <-chan Message { return c.send }

// NewClient constructs a client with a fresh send buffer.
func NewClient(userID domain.UUID, name string, docID domain.UUID) *Client {
	return &Client{UserID: userID, Name: name, DocID: docID}
}

// randomUUID kept to avoid unused import in some build configs.
var _ = uuid.New
