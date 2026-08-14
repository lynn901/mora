package main

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/infra/postgres"
	"github.com/lynn901/mora/internal/module/mora/collab"
	"github.com/lynn901/mora/internal/platform/auth"
	"github.com/lynn901/mora/internal/platform/authz"
	"github.com/lynn901/mora/internal/platform/rbac"
)

// newRBACEngine builds the RBAC engine from the postgres permission + directory
// + document repos via the RBACAdapter. Per design-docs/13 §3.5 (D2), the
// engine's internal target resolution delegates to a CompositeLocator backed
// by a DocLocator over the same repository — the doc path behavior is
// unchanged (regression red line: Check/VisibleDocuments + engine_test.go).
// Phase 1 adds the Asset + Source + Review locators (design-docs/14 §3.3/§8.2)
// so the authz layer can resolve asset/source/review targets the same way it
// resolves documents — a missing/cross-workspace asset or source, or missing
// review, returns ErrTargetNotFound, indistinguishable from a denial
// (existence never leaks).
func newRBACEngine(perms *postgres.PermissionRepo, dirs *postgres.DirectoryRepo, docs *postgres.DocumentRepo, authzDB *postgres.DB) *rbac.Engine {
	repo := postgres.NewRBACAdapter(perms, dirs, docs)
	eng := rbac.NewEngine(repo)
	// Wire doc-family + asset/source/review target resolution through the
	// unified ResourceLocator port. AsLocator adapts authz.ResourceLocator -> rbac.Locator
	// so the engine can delegate without importing authz (which imports rbac).
	comp := authz.NewCompositeLocator(struct {
		Type authz.TargetType
		Loc  authz.ResourceLocator
	}{Type: domain.TargetWorkspace, Loc: authz.NewDocLocator(repo)},
		struct {
			Type authz.TargetType
			Loc  authz.ResourceLocator
		}{Type: domain.TargetDirectory, Loc: authz.NewDocLocator(repo)},
		struct {
			Type authz.TargetType
			Loc  authz.ResourceLocator
		}{Type: domain.TargetDocument, Loc: authz.NewDocLocator(repo)},
		struct {
			Type authz.TargetType
			Loc  authz.ResourceLocator
		}{Type: domain.TargetAsset, Loc: authz.NewAssetLocator(postgres.NewAuthzAssetRepo(authzDB))},
		struct {
			Type authz.TargetType
			Loc  authz.ResourceLocator
		}{Type: domain.TargetSource, Loc: authz.NewSourceLocator(postgres.NewAuthzSourceRepo(authzDB))},
		struct {
			Type authz.TargetType
			Loc  authz.ResourceLocator
		}{Type: domain.TargetReview, Loc: authz.NewReviewLocator(postgres.NewAuthzReviewRepo(authzDB))},
	)
	eng.SetLocator(authz.AsLocator(comp))
	return eng
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
		_ = writeMsg(conn, collab.Message{Type: "denied", Payload: jsonRaw(`"` + res.Reason + `"`)})
		return
	}
	if res.ReadOnly {
		_ = writeMsg(conn, collab.Message{Type: "degraded", Payload: jsonRaw(`"` + res.Reason + `"`)})
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
