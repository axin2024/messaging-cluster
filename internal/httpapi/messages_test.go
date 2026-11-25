package httpapi

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket/wsjson"

	"github.com/axin2024/messaging-cluster/internal/credential"
	"github.com/axin2024/messaging-cluster/internal/store"
)

func TestConversationsAndDurableMessagesOnMySQL(t *testing.T) {
	dsn := testMySQLDSN(t)
	if _, err := store.Migrate(dsn); err != nil {
		t.Fatal(err)
	}
	storage, err := store.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { storage.Close() })
	server := gatewayTestServer(t, storage, "conversations")
	second := gatewayTestServer(t, storage, "conversations-second")
	f := identityFixture{handler: server.Config.Handler}
	tenant, admin := messageTestApp(t, storage)
	owner := messageTestUser(t, f, tenant, admin, "owner")
	phone := messageTestLogin(t, f, tenant, "owner", "phone")
	member := messageTestUser(t, f, tenant, admin, "member")
	manager := messageTestUser(t, f, tenant, admin, "manager")
	otherTenant, otherAdmin := messageTestApp(t, storage)
	outsider := messageTestUser(t, f, otherTenant, otherAdmin, "outsider")
	openDirect := func(user uint64) map[string]uint64 { return map[string]uint64{"user_id": user} }

	// Opposite initiation orders compete for one canonical pair.
	started := make(chan struct{})
	direct := make(chan *httptest.ResponseRecorder, 2)
	go func() {
		<-started
		direct <- f.request("POST", "/v1/conversations/direct", owner.Token, strings.NewReader(fmt.Sprintf(`{"user_id":%d}`, member.Session.UserID)))
	}()
	go func() {
		<-started
		direct <- f.request("POST", "/v1/conversations/direct", member.Token, strings.NewReader(fmt.Sprintf(`{"user_id":%d}`, owner.Session.UserID)))
	}()
	close(started)
	var directID uint64
	for range 2 {
		response := <-direct
		var conversation store.Conversation
		if err := json.Unmarshal(response.Body.Bytes(), &conversation); err != nil || response.Code != http.StatusOK || conversation.ID == 0 {
			t.Fatalf("open direct: HTTP %d %s", response.Code, response.Body)
		}
		if directID != 0 && directID != conversation.ID {
			t.Fatalf("duplicate direct conversation: %d and %d", directID, conversation.ID)
		}
		directID = conversation.ID
	}
	f.check(t, "POST", "/v1/conversations/direct", owner.Token, openDirect(outsider.Session.UserID), http.StatusNotFound, nil)

	var group store.Conversation
	f.check(t, "POST", "/v1/conversations/group", owner.Token, map[string]string{
		"title": "港口调度", "history_policy": "since_join",
	}, http.StatusCreated, &group)
	base := fmt.Sprintf("/v1/conversations/%d", group.ID)
	messages := base + "/messages"
	memberPath := fmt.Sprintf("%s/members/%d", base, member.Session.UserID)
	managerPath := fmt.Sprintf("%s/members/%d", base, manager.Session.UserID)
	ownerPath := fmt.Sprintf("%s/members/%d", base, owner.Session.UserID)
	role := func(name string) map[string]string { return map[string]string{"role": name} }
	payload := func(clientID, body string) map[string]string {
		return map[string]string{"client_id": clientID, "body": body}
	}
	f.check(t, "POST", messages, owner.Token, payload("before-join", "首班交接"), http.StatusCreated, nil)
	var membership store.Membership
	f.check(t, "PUT", managerPath, owner.Token, role("admin"), http.StatusOK, nil)
	f.check(t, "PUT", memberPath, manager.Token, role("member"), http.StatusOK, &membership)
	if membership.FromSeq != 2 {
		t.Fatalf("unexpected visibility start: %+v", membership)
	}
	f.check(t, "PUT", memberPath, manager.Token, role("admin"), http.StatusForbidden, nil)
	f.check(t, "DELETE", ownerPath, manager.Token, nil, http.StatusForbidden, nil)
	f.check(t, "DELETE", managerPath, member.Token, nil, http.StatusForbidden, nil)

	socket, oldConnection := dialGateway(t, server.URL, owner.Token, "")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	frame := map[string]any{"type": "message.send", "request_id": "ws-2", "conversation_id": group.ID,
		"client_id": "bridge-update", "body": "桥吊已经就位"}
	if err := wsjson.Write(ctx, socket, frame); err != nil {
		t.Fatal(err)
	}
	var accepted struct {
		Type      string               `json:"type"`
		RequestID string               `json:"request_id"`
		Error     string               `json:"error"`
		Receipt   store.MessageReceipt `json:"receipt"`
	}
	if err := wsjson.Read(ctx, socket, &accepted); err != nil || accepted.Type != "message.accepted" ||
		accepted.RequestID != "ws-2" || accepted.Receipt.Message.Seq != 2 || accepted.Receipt.Duplicate {
		t.Fatalf("unexpected delivery receipt: %+v, %v", accepted, err)
	}
	_, currentConnection := dialGateway(t, second.URL, owner.Token, "")
	digest, _ := credential.TokenDigest(owner.Token)
	currentSession, err := storage.Session(ctx, digest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.SendMessage(ctx, currentSession, &oldConnection, group.ID, store.OutgoingMessage{ClientID: "stale", Body: "旧连接"}); !errors.Is(err, store.ErrUnauthorized) {
		t.Fatalf("stale connection could write: %v", err)
	}
	if got, err := storage.SendMessage(ctx, currentSession, &currentConnection, group.ID, store.OutgoingMessage{ClientID: "bridge-update", Body: "桥吊已经就位"}); err != nil || !got.Duplicate {
		t.Fatalf("current connection could not replay: %+v, %v", got, err)
	}
	var receipt store.MessageReceipt
	f.check(t, "POST", messages, owner.Token, payload("bridge-update", "桥吊已经就位"), http.StatusOK, &receipt)
	if !reflect.DeepEqual(receipt.Message, accepted.Receipt.Message) || !receipt.Duplicate {
		t.Fatalf("replayed receipt differs: %+v and %+v", accepted.Receipt, receipt)
	}
	f.check(t, "POST", messages, owner.Token, payload("bridge-update", "争抢原消息"), http.StatusConflict, nil)
	directMessages := fmt.Sprintf("/v1/conversations/%d/messages", directID)
	f.check(t, "POST", directMessages, owner.Token, payload("bridge-update", "桥吊已经就位"), http.StatusConflict, nil)
	f.check(t, "GET", messages+"/by-client/bridge-update", phone.Token, nil, http.StatusNotFound, nil)
	var byClient store.Message
	f.check(t, "GET", messages+"/by-client/bridge-update", owner.Token, nil, http.StatusOK, &byClient)
	if !reflect.DeepEqual(byClient, receipt.Message) {
		t.Fatalf("client ID readback differs: %+v and %+v", byClient, receipt.Message)
	}
	f.check(t, "POST", messages, manager.Token, payload("bridge-update", "不同发送人"), http.StatusCreated, nil)

	assertMessagePage(t, f, messages+"?after=0&limit=1", member.Token, 2, true, 2)
	assertMessagePage(t, f, messages+"?after=2", member.Token, 3, false, 3)
	f.check(t, "GET", messages+"/1", member.Token, nil, http.StatusForbidden, nil)
	f.check(t, "DELETE", memberPath, member.Token, nil, http.StatusNoContent, nil)
	f.check(t, "POST", messages, owner.Token, payload("after-leave", "下一班交接"), http.StatusCreated, nil)
	f.check(t, "GET", messages+"/4", member.Token, nil, http.StatusForbidden, nil)
	f.check(t, "GET", messages+"/3", member.Token, nil, http.StatusOK, nil)
	f.check(t, "GET", base+"/members?after=999", member.Token, nil, http.StatusNotFound, nil)
	f.check(t, "POST", messages, member.Token, payload("not-active", "退群后发送"), http.StatusForbidden, nil)
	f.check(t, "GET", fmt.Sprintf("%s/2?tenant=%s", messages, tenant), outsider.Token, nil, http.StatusNotFound, nil)
	f.check(t, "POST", messages, outsider.Token, payload("other-tenant", "跨租户"), http.StatusNotFound, nil)
	var conversations page[store.Conversation]
	f.check(t, "GET", "/v1/conversations", member.Token, nil, http.StatusOK, &conversations)
	if len(conversations.Items) != 2 || conversations.Items[0].ID != directID || conversations.Items[1].ID != group.ID {
		t.Fatalf("conversation lookup lost scoped history: %+v", conversations)
	}

	f.check(t, "PUT", memberPath, owner.Token, role("member"), http.StatusOK, nil)
	f.check(t, "POST", messages, owner.Token, payload("after-rejoin", "重新加入"), http.StatusCreated, nil)
	assertMessagePage(t, f, messages, member.Token, 5, false, 2, 3, 5)
	f.check(t, "PUT", base+"/owner", owner.Token, openDirect(manager.Session.UserID), http.StatusNoContent, nil)
	f.check(t, "DELETE", managerPath, owner.Token, nil, http.StatusForbidden, nil)
	f.check(t, "DELETE", ownerPath, manager.Token, nil, http.StatusNoContent, nil)

	var full store.Conversation
	f.check(t, "POST", "/v1/conversations/group", manager.Token, map[string]string{
		"title": "交接记录", "history_policy": "all",
	}, http.StatusCreated, &full)
	fullBase := fmt.Sprintf("/v1/conversations/%d", full.ID)
	f.check(t, "POST", fullBase+"/messages", manager.Token, payload("prior", "群历史"), http.StatusCreated, nil)
	f.check(t, "PUT", fmt.Sprintf("%s/members/%d", fullBase, member.Session.UserID), manager.Token, role("member"), http.StatusOK, nil)
	assertMessagePage(t, f, fullBase+"/messages", member.Token, 1, false, 1)
	assertMessageOutbox(t, dsn, group.ID, 5)
	assertConcurrentSend(t, f, directID, owner.Token)
	f.check(t, "POST", directMessages, phone.Token, payload("same-send", "同一个编号由另一台设备生成"), http.StatusCreated, nil)
	assertMessageOutbox(t, dsn, directID, 2)
}

func assertConcurrentSend(t *testing.T, f identityFixture, conversation uint64, token string) {
	t.Helper()
	const attempts = 8
	path := fmt.Sprintf("/v1/conversations/%d/messages", conversation)
	started := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, attempts)
	for range attempts {
		go func() {
			<-started
			results <- f.request("POST", path, token, strings.NewReader(`{"client_id":"same-send","body":"同一条消息"}`))
		}()
	}
	close(started)
	created := 0
	for range attempts {
		response := <-results
		var receipt store.MessageReceipt
		if response.Code != http.StatusOK && response.Code != http.StatusCreated {
			t.Fatalf("concurrent send failed: HTTP %d %s", response.Code, response.Body)
		}
		if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil || receipt.Message.Seq != 1 ||
			receipt.Duplicate != (response.Code == http.StatusOK) {
			t.Fatalf("unexpected concurrent send receipt: %+v, error %v", receipt, err)
		}
		if response.Code == http.StatusCreated {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("concurrent send created %d messages", created)
	}
}

func messageTestApp(t *testing.T, storage *store.Store) (string, string) {
	t.Helper()
	token, digest, err := credential.IssueToken()
	if err != nil {
		t.Fatal(err)
	}
	tenant := "messages-" + hex.EncodeToString(digest[:6])
	if _, err := storage.CreateApp(t.Context(), tenant, "dispatch", digest); err != nil {
		t.Fatal(err)
	}
	return tenant, token
}

const messageTestPassword = "synthetic-message-password"

func messageTestUser(t *testing.T, f identityFixture, tenant, admin, user string) sessionResponse {
	t.Helper()
	f.check(t, "POST", "/v1/admin/users", admin, map[string]string{
		"username": user, "display_name": user, "password": messageTestPassword,
	}, http.StatusCreated, nil)
	return messageTestLogin(t, f, tenant, user, "desktop")
}

func messageTestLogin(t *testing.T, f identityFixture, tenant, user, device string) sessionResponse {
	t.Helper()
	var result sessionResponse
	f.check(t, "POST", "/v1/sessions", "", map[string]string{
		"tenant": tenant, "app": "dispatch", "username": user, "password": messageTestPassword, "device_key": device,
	}, http.StatusCreated, &result)
	return result
}

func assertMessagePage(t *testing.T, f identityFixture, path, token string, after uint64, more bool, seqs ...uint64) {
	t.Helper()
	var result struct {
		Items     []store.Message `json:"items"`
		NextAfter uint64          `json:"next_after"`
		HasMore   bool            `json:"has_more"`
	}
	f.check(t, "GET", path, token, nil, http.StatusOK, &result)
	if len(result.Items) != len(seqs) || result.NextAfter != after || result.HasMore != more {
		t.Fatalf("unexpected message page: %+v", result)
	}
	for index, seq := range seqs {
		if result.Items[index].Seq != seq {
			t.Fatalf("page item %d has sequence %d, expected %d", index, result.Items[index].Seq, seq)
		}
	}
}

func assertMessageOutbox(t *testing.T, dsn string, conversation uint64, count int) {
	t.Helper()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var stored, planned int
	err = db.QueryRowContext(t.Context(), `SELECT COUNT(*), COUNT(o.id) FROM messages AS m
		LEFT JOIN message_outbox AS o ON o.conversation_id = m.conversation_id AND o.seq = m.seq
		WHERE m.conversation_id = ?`, conversation).Scan(&stored, &planned)
	if err != nil || stored != count || planned != count {
		t.Fatalf("message/outbox row count: %d/%d, expected %d + %d, error %v", stored, planned, count, count, err)
	}
}
