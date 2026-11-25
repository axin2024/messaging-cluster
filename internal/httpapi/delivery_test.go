package httpapi

import (
	"context"
	"database/sql"
	"fmt"
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

func TestRealtimeDeliveryAndSlowPeerIsolationOnMySQL(t *testing.T) {
	storage := syncTestStore(t, testMySQLDSN(t))
	first := gatewayTestServer(t, storage, "delivery-sender")
	second := gatewayTestServer(t, storage, "delivery-receivers")
	f := identityFixture{handler: first.Config.Handler}
	tenant, admin := messageTestApp(t, storage)
	_, outsideAdmin := messageTestApp(t, storage)
	sender := messageTestUser(t, f, tenant, admin, "sender")
	slow := messageTestUser(t, f, tenant, admin, "slow")
	healthy := messageTestUser(t, f, tenant, admin, "healthy")
	slowChat := openDeliveryChat(t, f, sender, slow)
	healthyChat := openDeliveryChat(t, f, sender, healthy)
	slowSocket, _ := dialGateway(t, second.URL, slow.Token, "")
	healthySocket, _ := dialGateway(t, second.URL, healthy.Token, "")
	// Send from a different gateway than both receivers. Only the healthy peer grants credit.
	writer, _ := dialGateway(t, first.URL, sender.Token, "")
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	const count = gateway.DeliveryWindow + 5
	const diagnostics = "/v1/admin/diagnostics"
	var health store.Diagnostics
	for seq := uint64(1); seq <= count; seq++ {
		for _, chat := range []store.Conversation{slowChat, healthyChat} {
			id := fmt.Sprintf("chat-%d-seq-%d", chat.ID, seq)
			reply := sendDeliveryMessage(t, ctx, writer, chat.ID, id)
			if reply.Type != "message.accepted" || reply.Receipt == nil || reply.Receipt.Message.Seq != seq {
				t.Fatalf("sequence %d in chat %d: %+v", seq, chat.ID, reply)
			}
		}
		var event deliveryFrame
		if err := wsjson.Read(ctx, healthySocket, &event); err != nil {
			t.Fatal(err)
		}
		ackDelivery(t, ctx, healthySocket, event, "message", healthyChat.ID, seq)
		if seq == gateway.DeliveryWindow {
			for {
				f.check(t, "GET", diagnostics, admin, nil, http.StatusOK, &health)
				if len(health.Nodes) == 2 && health.Nodes[1].PendingDeliveries == gateway.DeliveryWindow && health.Nodes[1].OldestPendingMillis > 0 {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatalf("the slow node's backlog did not appear: %+v", health)
				case <-time.After(20 * time.Millisecond):
				}
			}
		}
	}
	for {
		var event deliveryFrame
		if err := wsjson.Read(ctx, slowSocket, &event); err != nil {
			t.Fatal(err)
		}
		if event.Type == "notification.resync_required" {
			break
		}
		if event.Type != "notification.message" || event.ConversationID != slowChat.ID || event.Seq > gateway.DeliveryWindow {
			t.Fatalf("unexpected slow peer frame: %+v", event)
		}
	}
	for {
		f.check(t, "GET", diagnostics, admin, nil, http.StatusOK, &health)
		if health.Delivery.Acked == count && health.Delivery.Resync == count && health.PendingOutbox == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out awaiting delivery diagnostics: %+v", health)
		case <-time.After(20 * time.Millisecond):
		}
	}
	if health.CommittedMessages != count*2 || len(health.Nodes) != 2 || health.Nodes[0].Active != 1 || health.Nodes[1].Active != 2 ||
		!health.Nodes[0].Healthy || !health.Nodes[1].Healthy || health.OldestOutboxMillis != 0 {
		t.Fatalf("unexpected diagnostics: %+v", health)
	}
	var status store.MessageDelivery
	path := fmt.Sprintf("/v1/admin/conversations/%d/messages/%d/delivery", slowChat.ID, count)
	f.check(t, "GET", path, admin, nil, http.StatusOK, &status)
	if status.Seq != count || !status.LiveDelivery || status.CommittedAt.IsZero() || status.DispatchedAt == nil || status.Delivery.Resync != 1 {
		t.Fatalf("unexpected per-message timeline: %+v", status)
	}
	f.check(t, "GET", path, outsideAdmin, nil, http.StatusNotFound, nil)
	path = fmt.Sprintf("/v1/admin/conversations/%d/messages/%d/delivery", healthyChat.ID, count)
	f.check(t, "GET", path, admin, nil, http.StatusOK, &status)
	if status.DispatchedAt == nil || status.FirstSentAt == nil || status.FirstAckedAt == nil || status.Delivery.Acked != 1 ||
		status.DispatchedAt.Before(status.CommittedAt) || status.FirstSentAt.Before(*status.DispatchedAt) || status.FirstAckedAt.Before(*status.FirstSentAt) {
		t.Fatalf("unexpected healthy message timeline: %+v", status)
	}
	f.check(t, "GET", diagnostics, slow.Token, nil, http.StatusUnauthorized, nil)
	var sync store.SyncPage
	f.check(t, "GET", fmt.Sprintf("/v1/conversations/%d/sync", slowChat.ID), slow.Token, nil, http.StatusOK, &sync)
	seqs := make([]uint64, count)
	for i := range seqs {
		seqs[i] = uint64(i + 1)
	}
	assertSyncPage(t, sync, count, false, seqs...)
	assertGatewayPong(t, slowSocket)

	base := fmt.Sprintf("/v1/conversations/%d", healthyChat.ID)
	phone := messageTestLogin(t, f, tenant, "healthy", "phone")
	f.check(t, "PUT", base+"/read", phone.Token, map[string]uint64{"seq": count}, http.StatusOK, nil)
	var event deliveryFrame
	if err := wsjson.Read(ctx, healthySocket, &event); err != nil {
		t.Fatal(err)
	}
	ackDelivery(t, ctx, healthySocket, event, "read", healthyChat.ID, count)
	f.check(t, "DELETE", fmt.Sprintf("%s/messages/%d", base, count), sender.Token, nil, http.StatusOK, nil)
	if err := wsjson.Read(ctx, healthySocket, &event); err != nil {
		t.Fatal(err)
	}
	ackDelivery(t, ctx, healthySocket, event, "retraction", healthyChat.ID, count)
}

type deliveryFrame struct {
	Type string `json:"type"`
	store.Delivery
}

func ackDelivery(t *testing.T, ctx context.Context, socket *websocket.Conn, frame deliveryFrame, kind string, conversationID, seq uint64) {
	t.Helper()
	if frame.Type != "notification."+kind || frame.ID == 0 || frame.ConversationID != conversationID || frame.Seq != seq {
		t.Fatalf("unexpected %s hint for conversation %d, sequence %d: %+v", kind, conversationID, seq, frame)
	}
	if err := wsjson.Write(ctx, socket, map[string]any{"type": "notification.ack", "delivery_id": frame.ID}); err != nil {
		t.Fatal(err)
	}
}

type deliveryReply struct {
	Type    string                `json:"type"`
	Error   string                `json:"error"`
	Receipt *store.MessageReceipt `json:"receipt"`
}

func sendDeliveryMessage(t *testing.T, ctx context.Context, socket *websocket.Conn, chat uint64, id string) deliveryReply {
	t.Helper()
	frame := map[string]any{"type": "message.send", "request_id": id, "conversation_id": chat, "client_id": id, "body": "Synthetic dispatch"}
	if err := wsjson.Write(ctx, socket, frame); err != nil {
		t.Fatal(err)
	}
	var reply deliveryReply
	if err := readGatewayReply(ctx, socket, id, &reply); err != nil {
		t.Fatal(err)
	}
	return reply
}

func openDeliveryChat(t *testing.T, f identityFixture, sender, receiver sessionResponse) store.Conversation {
	t.Helper()
	var chat store.Conversation
	f.check(t, "POST", "/v1/conversations/direct", sender.Token, map[string]uint64{"user_id": receiver.Session.UserID}, http.StatusOK, &chat)
	return chat
}

func TestUnconfirmedMessageDoesNotProduceReceiptOnMySQL(t *testing.T) {
	dsn := testMySQLDSN(t)
	storage := syncTestStore(t, dsn)
	server := gatewayTestServer(t, storage, "unconfirmed")
	f := identityFixture{handler: server.Config.Handler}
	tenant, admin := messageTestApp(t, storage)
	sender := messageTestUser(t, f, tenant, admin, "sender")
	receiver := messageTestUser(t, f, tenant, admin, "receiver")
	chat := openDeliveryChat(t, f, sender, receiver)
	socket, _ := dialGateway(t, server.URL, sender.Token, "")
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	blocked, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocked.Close()
	// Hold the synthetic fixture's outbox past the WebSocket transaction deadline.
	if _, err := blocked.ExecContext(ctx, `LOCK TABLES message_outbox WRITE`); err != nil {
		t.Fatal(err)
	}
	reply := sendDeliveryMessage(t, ctx, socket, chat.ID, "retry-after-lock")
	if _, err := blocked.ExecContext(ctx, `UNLOCK TABLES`); err != nil {
		t.Fatal(err)
	}
	if reply.Type != "error" || reply.Error != "message_unconfirmed" || reply.Receipt != nil {
		t.Fatalf("a blocked transaction produced a receipt: %+v", reply)
	}
	path := fmt.Sprintf("/v1/conversations/%d/messages/by-client/retry-after-lock", chat.ID)
	f.check(t, "GET", path, sender.Token, nil, http.StatusNotFound, nil)
	var stats store.Diagnostics
	f.check(t, "GET", "/v1/admin/diagnostics", admin, nil, http.StatusOK, &stats)
	if stats.CommittedMessages != 0 || stats.PendingOutbox != 0 || stats.Delivery != (store.DeliveryCounts{}) {
		t.Fatalf("unconfirmed write left durable state: %+v", stats)
	}
	reply = sendDeliveryMessage(t, ctx, socket, chat.ID, "retry-after-lock")
	if reply.Type != "message.accepted" || reply.Receipt == nil || reply.Receipt.Message.Seq != 1 || reply.Receipt.Duplicate {
		t.Fatalf("retry after unlock: %+v", reply)
	}
	assertMessageOutbox(t, dsn, chat.ID, 1)
}

func TestTenantRateQuotaAcrossAppsAndGatewaysOnMySQL(t *testing.T) {
	dsn := testMySQLDSN(t)
	storage := syncTestStore(t, dsn)
	first := gatewayTestServer(t, storage, "quota-first")
	second := gatewayTestServer(t, storage, "quota-second")
	f := identityFixture{handler: first.Config.Handler}
	f2 := identityFixture{handler: second.Config.Handler}
	tenant, admin := messageTestApp(t, storage)
	if err := storage.SetTenantSendLimit(t.Context(), tenant, 1); err != nil {
		t.Fatal(err)
	}
	sender := messageTestUser(t, f, tenant, admin, "sender")
	receiver := messageTestUser(t, f, tenant, admin, "receiver")
	firstChat := openDeliveryChat(t, f, sender, receiver)
	secondAdmin, digest, err := credential.IssueToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CreateApp(t.Context(), tenant, "second", digest); err != nil {
		t.Fatal(err)
	}
	secondUser := func(user string) sessionResponse {
		f2.check(t, "POST", "/v1/admin/users", secondAdmin, map[string]string{
			"username": user, "display_name": user, "password": messageTestPassword,
		}, http.StatusCreated, nil)
		var session sessionResponse
		f2.check(t, "POST", "/v1/sessions", "", map[string]string{
			"tenant": tenant, "app": "second", "username": user, "password": messageTestPassword, "device_key": "desktop",
		}, http.StatusCreated, &session)
		return session
	}
	otherSender := secondUser("sender")
	secondChat := openDeliveryChat(t, f2, otherSender, secondUser("receiver"))
	outTenant, outAdmin := messageTestApp(t, storage)
	outSender := messageTestUser(t, f2, outTenant, outAdmin, "sender")
	outChat := openDeliveryChat(t, f2, outSender, messageTestUser(t, f2, outTenant, outAdmin, "receiver"))
	type route struct {
		fixture identityFixture
		token   string
		path    string
	}
	routes := [2]route{
		{f, sender.Token, fmt.Sprintf("/v1/conversations/%d/messages", firstChat.ID)},
		{f2, otherSender.Token, fmt.Sprintf("/v1/conversations/%d/messages", secondChat.ID)},
	}
	type attempt struct {
		route
		body     string
		response *httptest.ResponseRecorder
	}
	const requests = 12
	started := make(chan struct{})
	results := make(chan attempt, requests)
	for i := range requests {
		go func() {
			target := routes[i%len(routes)]
			body := fmt.Sprintf(`{"client_id":"quota-%d","body":"Synthetic dispatch"}`, i)
			<-started
			results <- attempt{target, body, target.fixture.request("POST", target.path, target.token, strings.NewReader(body))}
		}()
	}
	close(started)
	var accepted []attempt
	for range requests {
		next := <-results
		switch next.response.Code {
		case http.StatusCreated:
			accepted = append(accepted, next)
		case http.StatusTooManyRequests:
			if next.response.Header().Get("Retry-After") != "1" || !strings.Contains(next.response.Body.String(), "tenant_rate_limited") {
				t.Fatalf("quota response: %v %s", next.response.Header(), next.response.Body)
			}
		default:
			t.Fatalf("send returned HTTP %d: %s", next.response.Code, next.response.Body)
		}
	}
	if len(accepted) == 0 || len(accepted) == requests {
		t.Fatalf("expected both accepts and throttles, accepted %d/%d", len(accepted), requests)
	}
	for _, original := range accepted {
		replay := f2.request("POST", original.path, original.token, strings.NewReader(original.body))
		if replay.Code != http.StatusOK || !strings.Contains(replay.Body.String(), `"duplicate":true`) {
			t.Fatalf("quota denied a replay: HTTP %d %s", replay.Code, replay.Body)
		}
	}
	f2.check(t, "POST", fmt.Sprintf("/v1/conversations/%d/messages", outChat.ID), outSender.Token, syncTestMessage(1), http.StatusCreated, nil)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var buckets, total, biggest int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*), COALESCE(SUM(b.accepted), 0), COALESCE(MAX(b.accepted), 0)
		FROM tenant_send_buckets AS b JOIN tenants AS t ON t.id = b.tenant_id WHERE t.tenant_key = ?`, tenant).
		Scan(&buckets, &total, &biggest); err != nil {
		t.Fatal(err)
	}
	if biggest != 1 || buckets != len(accepted) || total != len(accepted) {
		t.Fatalf("one shared quota should match only persisted messages: %d buckets, %d charged, max %d, %d accepted", buckets, total, biggest, len(accepted))
	}
}
