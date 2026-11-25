package httpapi

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/axin2024/messaging-cluster/internal/credential"
	"github.com/axin2024/messaging-cluster/internal/gateway"
	"github.com/axin2024/messaging-cluster/internal/store"
)

func TestGatewayHandoffOnMySQL(t *testing.T) {
	dsn := testMySQLDSN(t)
	if _, err := store.Migrate(dsn); err != nil {
		t.Fatal(err)
	}
	storage, err := store.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { storage.Close() })

	first := gatewayTestServer(t, storage, "first")
	second := gatewayTestServer(t, storage, "second")
	fixture := identityFixture{handler: first.Config.Handler}
	admin, digest, err := credential.IssueToken()
	if err != nil {
		t.Fatal(err)
	}
	tenant := "gateway-" + hex.EncodeToString(digest[:6])
	if _, err := storage.CreateApp(t.Context(), tenant, "dispatch", digest); err != nil {
		t.Fatal(err)
	}
	const password = "synthetic-operator-password"
	fixture.check(t, "POST", "/v1/admin/users", admin, map[string]string{
		"username": "operator", "display_name": "Operator", "password": password,
	}, http.StatusCreated, nil)
	login := func(device string) sessionResponse {
		t.Helper()
		var response sessionResponse
		fixture.check(t, "POST", "/v1/sessions", "", map[string]string{
			"tenant": tenant, "app": "dispatch", "username": "operator", "password": password, "device_key": device,
		}, http.StatusCreated, &response)
		return response
	}
	phone, desktop := login("phone"), login("desktop")
	oldSocket, oldConnection := dialGateway(t, first.URL, phone.Token, "")
	desktopSocket, _ := dialGateway(t, first.URL, desktop.Token, "")
	var ticket struct {
		Ticket string `json:"ticket"`
	}
	fixture.check(t, "POST", "/v1/gateway/tickets", phone.Token, nil, http.StatusCreated, &ticket)
	newSocket, current := dialGateway(t, second.URL, "", ticket.Ticket)
	if current.DeviceID != oldConnection.DeviceID || current.Epoch != oldConnection.Epoch+1 || current.Node.ID == oldConnection.Node.ID {
		t.Fatalf("unexpected handoff from %+v to %+v", oldConnection, current)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	reused, response, err := websocket.Dial(ctx, gatewayURL(second.URL), &websocket.DialOptions{
		Subprotocols: []string{gateway.Protocol, "ticket." + ticket.Ticket},
	})
	if err == nil {
		_ = reused.CloseNow()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("reused ticket should be rejected with 401: response %v, error %v", response, err)
	}
	if err := storage.Disconnect(ctx, oldConnection, "client_closed"); err != nil {
		t.Fatal(err)
	}
	assertSocketClosed(t, oldSocket)
	phonePath := fmt.Sprintf("/v1/devices/%d", phone.Session.DeviceID)
	var presence store.Presence
	fixture.check(t, "GET", phonePath+"/presence", desktop.Token, nil, http.StatusOK, &presence)
	if !presence.Online || presence.Connection == nil || presence.Connection.Epoch != current.Epoch {
		t.Fatalf("late close changed current connection: %+v", presence)
	}
	var history struct {
		Items []store.Connection `json:"items"`
	}
	fixture.check(t, "GET", phonePath+"/connections", desktop.Token, nil, http.StatusOK, &history)
	if len(history.Items) != 2 || history.Items[0].Epoch != current.Epoch ||
		history.Items[1].Epoch != oldConnection.Epoch || history.Items[1].CloseReason != "superseded" {
		t.Fatalf("unexpected connection history: %+v", history.Items)
	}
	assertGatewayPong(t, newSocket)
	fixture.check(t, "DELETE", phonePath, desktop.Token, nil, http.StatusNoContent, nil)
	assertSocketClosed(t, newSocket)
	presence = store.Presence{}
	fixture.check(t, "GET", phonePath+"/presence", desktop.Token, nil, http.StatusOK, &presence)
	if presence.Online || presence.Status != "session_revoked" {
		t.Fatalf("revoked device remains online: %+v", presence)
	}
	assertGatewayPong(t, desktopSocket)
}

type gatewayFixture struct {
	*httptest.Server
	node *gateway.Gateway
}

func gatewayTestServer(t *testing.T, storage *store.Store, name string) *gatewayFixture {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	config := gateway.DefaultConfig()
	config.Reconcile = 40 * time.Millisecond
	node, err := gateway.Start(t.Context(), storage, name, config, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := node.Close(); err != nil {
			t.Error(err)
		}
	})
	handler, err := New(storage, node, logger)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &gatewayFixture{Server: server, node: node}
}

func gatewayURL(origin string) string {
	return "ws" + strings.TrimPrefix(origin, "http") + "/v1/gateway"
}

func dialGateway(t *testing.T, origin, token, ticket string) (*websocket.Conn, store.Connection) {
	t.Helper()
	options := &websocket.DialOptions{HTTPHeader: make(http.Header), Subprotocols: []string{gateway.Protocol}}
	if ticket != "" {
		options.HTTPHeader.Set("Origin", origin)
		options.Subprotocols = append(options.Subprotocols, "ticket."+ticket)
	} else {
		options.HTTPHeader.Set("Authorization", "Bearer "+token)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	socket, _, err := websocket.Dial(ctx, gatewayURL(origin), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = socket.CloseNow() })
	var ready struct {
		Type string `json:"type"`
		store.Connection
	}
	if err := wsjson.Read(ctx, socket, &ready); err != nil || ready.Type != "ready" || socket.Subprotocol() != gateway.Protocol {
		t.Fatalf("unexpected gateway handshake: %+v, error %v", ready, err)
	}
	return socket, ready.Connection
}

func assertSocketClosed(t *testing.T, socket *websocket.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if _, _, err := socket.Read(ctx); err == nil || ctx.Err() != nil {
		t.Fatalf("socket did not close before test deadline: %v", err)
	}
}

func assertGatewayPong(t *testing.T, socket *websocket.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, socket, map[string]string{"type": "ping", "request_id": "health"}); err != nil {
		t.Fatal(err)
	}
	var pong map[string]string
	if err := wsjson.Read(ctx, socket, &pong); err != nil || pong["type"] != "pong" || pong["request_id"] != "health" {
		t.Fatalf("unexpected gateway heartbeat: %v, error %v", pong, err)
	}
}
