package main

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/wiki/wiki-backend/internal/domain"
	"github.com/wiki/wiki-backend/internal/infra/postgres"
	"github.com/wiki/wiki-backend/internal/module/wiki/collab"
	"github.com/wiki/wiki-backend/internal/platform/auth"
	"github.com/wiki/wiki-backend/internal/platform/rbac"
)

// newRBACEngine builds the RBAC engine from the postgres permission + directory
// repos via the RBACAdapter.
func newRBACEngine(perms *postgres.PermissionRepo, dirs *postgres.DirectoryRepo) *rbac.Engine {
	return rbac.NewEngine(postgres.NewRBACAdapter(perms, dirs))
}

var collabUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true }, // production: restrict origin
}

// serveCollab handles the collaboration WebSocket: authenticates the joiner,
// registers presence (with concurrency-limit degradation), and relays
// presence/cursor messages. Full Yjs CRDT sync is delegated to yjs-server
// (Node.js); this Go endpoint owns admission control + presence relay.
func serveCollab(c *gin.Context, hub *collab.Hub, tm *auth.TokenManager) {
	token := c.Query("token")
	claims, err := tm.Verify(token)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"code": 40100, "message": "invalid token"})
		return
	}
	docID, err := uuid.Parse(c.Param("document_id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 40000, "message": "invalid document_id"})
		return
	}
	uid, err := uuid.Parse(claims.UserID)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"code": 40100, "message": "invalid identity"})
		return
	}

	conn, err := collabUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	client := collab.NewClient(uid, claims.Name, docID)
	res, roster := hub.Register(client)
	if !res.Admitted {
		_ = writeMsg(conn, collab.Message{Type: "denied", Payload: jsonRaw(`"`+res.Reason+`"`)})
		return
	}
	if res.ReadOnly {
		_ = writeMsg(conn, collab.Message{Type: "degraded", Payload: jsonRaw(`"`+res.Reason+`"`)})
	}
	// broadcast join + send roster
	hub.Broadcast(docID, collab.Message{Type: "join", From: uid})
	_ = writeMsg(conn, collab.Message{Type: "presence", Payload: mustJSON(roster)})
	defer func() {
		hub.Unregister(docID, uid)
		hub.Broadcast(docID, collab.Message{Type: "leave", From: uid})
	}()

	// writer: drain client.Send() to the WebSocket
	done := make(chan struct{})
	go func() {
		defer close(done)
		for msg := range client.Send() {
			if err := writeMsg(conn, msg); err != nil {
				return
			}
		}
	}()

	// reader: read incoming presence/cursor messages until close
	for {
		var msg collab.Message
		if err := conn.ReadJSON(&msg); err != nil {
			break
		}
		switch msg.Type {
		case "cursor":
			var cur collab.Cursor
			if len(msg.Payload) > 0 {
				_ = json.Unmarshal(msg.Payload, &cur)
			}
			hub.UpdateCursor(docID, uid, cur)
		case "update":
			// content update; relay to other collaborators (CRDT handled by yjs-server)
			hub.Broadcast(docID, collab.Message{Type: "update", From: uid, Payload: msg.Payload})
		}
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
}

func writeMsg(conn *websocket.Conn, msg collab.Message) error {
	return conn.WriteJSON(msg)
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func jsonRaw(s string) []byte { return []byte(s) }

// domain import retained for future per-doc RBAC checks on collab join.
var _ = domain.UUID{}
