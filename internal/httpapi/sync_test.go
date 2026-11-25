package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket/wsjson"

	"github.com/axin2024/messaging-cluster/internal/store"
)

func TestOfflineSyncAndReadAcrossGatewaysOnMySQL(t *testing.T) {
	dsn := testMySQLDSN(t)
	storage := syncTestStore(t, dsn)
	first := gatewayTestServer(t, storage, "sync-first")
	second := gatewayTestServer(t, storage, "sync-second")
	f := identityFixture{handler: first.Config.Handler}
	f2 := identityFixture{handler: second.Config.Handler}
	tenant, admin := messageTestApp(t, storage)
	sender := messageTestUser(t, f, tenant, admin, "sender")
	desktop := messageTestUser(t, f, tenant, admin, "receiver")
	phone := messageTestLogin(t, f, tenant, "receiver", "phone")
	var group store.Conversation
	f.check(t, "POST", "/v1/conversations/group", sender.Token, map[string]string{
		"title": "Shift handoff", "history_policy": "since_join",
	}, http.StatusCreated, &group)
	base := fmt.Sprintf("/v1/conversations/%d", group.ID)
	memberPath := fmt.Sprintf("%s/members/%d", base, desktop.Session.UserID)
	f.check(t, "PUT", memberPath, sender.Token, map[string]string{"role": "member"}, http.StatusOK, nil)
	messagePath := base + "/messages"
	syncPath := base + "/sync"
	readPath := base + "/read"
	for seq := 1; seq <= 40; seq++ {
		var receipt store.MessageReceipt
		f.check(t, "POST", messagePath, sender.Token, syncTestMessage(seq), http.StatusCreated, &receipt)
		if receipt.Message.Seq != uint64(seq) {
			t.Fatalf("sequence %d was assigned to message %d", receipt.Message.Seq, seq)
		}
	}
	checkpoint := func(seq uint64) map[string]uint64 { return map[string]uint64{"seq": seq} }
	f.check(t, "PUT", syncPath, phone.Token, checkpoint(40), http.StatusOK, nil)
	f.check(t, "PUT", readPath, phone.Token, checkpoint(40), http.StatusOK, nil)
	f.check(t, "PUT", syncPath, desktop.Token, checkpoint(35), http.StatusOK, nil)
	old, earlier := dialGateway(t, first.URL, phone.Token, "")
	writer, _ := dialGateway(t, first.URL, sender.Token, "")
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, writer, map[string]any{"type": "message.send", "conversation_id": group.ID,
		"request_id": "lost-receipt", "client_id": "batch-41", "body": "Message 41"}); err != nil {
		t.Fatal(err)
	}
	// Observe the durable write without reading the socket's receipt, then shut down G1.
	for {
		response := f.request("GET", messagePath+"/by-client/batch-41", sender.Token, nil)
		if response.Code == http.StatusOK {
			break
		}
		if response.Code != http.StatusNotFound || ctx.Err() != nil {
			t.Fatalf("message 41 was not persisted before shutdown: HTTP %d", response.Code)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := first.node.Close(); err != nil {
		t.Fatal(err)
	}
	first.Close()
	assertSocketClosed(t, old)
	var retry store.MessageReceipt
	f2.check(t, "POST", messagePath, sender.Token, syncTestMessage(41), http.StatusOK, &retry)
	if !retry.Duplicate || retry.Message.Seq != 41 {
		t.Fatalf("retry did not resolve the unread receipt: %+v", retry)
	}
	f2.check(t, "POST", messagePath, sender.Token, syncTestMessage(42), http.StatusCreated, nil)
	_, current := dialGateway(t, second.URL, phone.Token, "")
	if current.Epoch != earlier.Epoch+1 || current.Node.ID == earlier.Node.ID {
		t.Fatalf("phone did not reconnect across gateways: %+v -> %+v", earlier, current)
	}
	var inbox page[store.InboxEntry]
	getFromGateway(t, second, "/v1/inbox", phone.Token, &inbox)
	if len(inbox.Items) != 1 || inbox.Items[0] != (store.InboxEntry{ConversationID: group.ID, HighSeq: 42, SyncSeq: 40, ReadSeq: 40}) {
		t.Fatalf("phone inbox did not retain its positions: %+v", inbox.Items)
	}
	var messages store.SyncPage
	getFromGateway(t, second, syncPath+"?limit=1", phone.Token, &messages)
	assertSyncPage(t, messages, 41, true, 41)
	getFromGateway(t, second, syncPath, phone.Token, &messages)
	assertSyncPage(t, messages, 42, false, 41, 42)
	f2.check(t, "PUT", syncPath, phone.Token, checkpoint(41), http.StatusOK, nil)
	getFromGateway(t, second, syncPath, phone.Token, &messages)
	assertSyncPage(t, messages, 42, false, 42)
	f2.check(t, "PUT", syncPath, phone.Token, checkpoint(42), http.StatusOK, nil)
	getFromGateway(t, second, syncPath, phone.Token, &messages)
	assertSyncPage(t, messages, 42, false)
	f2.check(t, "PUT", syncPath, phone.Token, checkpoint(39), http.StatusOK, nil)
	getFromGateway(t, second, "/v1/inbox", desktop.Token, &inbox)
	if len(inbox.Items) != 1 || inbox.Items[0].SyncSeq != 35 {
		t.Fatalf("sync acknowledgement leaked to another device: %+v", inbox.Items)
	}

	assertReadState(t, f2, readPath, desktop.Token, 40, 2)
	f2.check(t, "PUT", readPath, phone.Token, checkpoint(42), http.StatusOK, nil)
	assertReadState(t, f2, readPath, desktop.Token, 42, 0)
	f2.check(t, "PUT", readPath, desktop.Token, checkpoint(40), http.StatusOK, nil)
	assertReadState(t, f2, readPath, phone.Token, 42, 0)
	f2.check(t, "PUT", readPath, desktop.Token, checkpoint(43), http.StatusForbidden, nil)
	f2.check(t, "POST", messagePath, desktop.Token, syncTestMessage(43), http.StatusCreated, nil)
	assertReadState(t, f2, readPath, phone.Token, 42, 0)
	f2.check(t, "POST", messagePath, sender.Token, syncTestMessage(44), http.StatusCreated, nil)
	assertReadState(t, f2, readPath, phone.Token, 42, 1)
	f2.check(t, "DELETE", memberPath, phone.Token, nil, http.StatusNoContent, nil)
	f2.check(t, "POST", messagePath, sender.Token, syncTestMessage(45), http.StatusCreated, nil)
	f2.check(t, "GET", messagePath+"/45", phone.Token, nil, http.StatusForbidden, nil)
	getFromGateway(t, second, syncPath, phone.Token, &messages)
	assertSyncPage(t, messages, 44, false, 43, 44)
}

func TestRetentionReportsOnlyAuthorizedGapsOnMySQL(t *testing.T) {
	dsn := testMySQLDSN(t)
	storage := syncTestStore(t, dsn)
	server := gatewayTestServer(t, storage, "retention")
	f := identityFixture{handler: server.Config.Handler}
	tenant, admin := messageTestApp(t, storage)
	owner := messageTestUser(t, f, tenant, admin, "owner")
	member := messageTestUser(t, f, tenant, admin, "member")
	_, outsideAdmin := messageTestApp(t, storage)
	var group store.Conversation
	f.check(t, "POST", "/v1/conversations/group", owner.Token, map[string]string{"title": "Archive", "history_policy": "since_join"}, http.StatusCreated, &group)
	base := fmt.Sprintf("/v1/conversations/%d", group.ID)
	messages := base + "/messages"
	memberPath := fmt.Sprintf("%s/members/%d", base, member.Session.UserID)
	memberRole := map[string]string{"role": "member"}
	f.check(t, "POST", messages, owner.Token, syncTestMessage(1), http.StatusCreated, nil)
	f.check(t, "PUT", memberPath, owner.Token, memberRole, http.StatusOK, nil)
	f.check(t, "POST", messages, owner.Token, syncTestMessage(2), http.StatusCreated, nil)
	f.check(t, "DELETE", memberPath, member.Token, nil, http.StatusNoContent, nil)
	f.check(t, "POST", messages, owner.Token, syncTestMessage(3), http.StatusCreated, nil)
	f.check(t, "PUT", memberPath, owner.Token, memberRole, http.StatusOK, nil)
	f.check(t, "POST", messages, owner.Token, syncTestMessage(4), http.StatusCreated, nil)
	f.check(t, "POST", messages, owner.Token, syncTestMessage(5), http.StatusCreated, nil)

	// Age only the fixture prefix so the test has a stable cutoff and live suffix.
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.ExecContext(t.Context(), `UPDATE messages SET created_at = UTC_TIMESTAMP(6) - INTERVAL 7 DAY
		WHERE conversation_id = ? AND seq <= 4`, group.ID); err != nil {
		t.Fatal(err)
	}
	retention := fmt.Sprintf("/v1/admin/conversations/%d/retention", group.ID)
	cutoff := map[string]time.Time{"before": time.Now().UTC().Add(-24 * time.Hour)}
	f.check(t, "POST", retention, owner.Token, cutoff, http.StatusUnauthorized, nil)
	f.check(t, "POST", retention, outsideAdmin, cutoff, http.StatusNotFound, nil)
	var applied store.RetentionResult
	f.check(t, "POST", retention, admin, cutoff, http.StatusOK, &applied)
	if applied.HasMore || applied.RetainedFrom != 5 || applied.Run == nil || applied.Run.From != 1 || applied.Run.To != 4 {
		t.Fatalf("unexpected retention record: %+v", applied)
	}
	var history page[store.RetentionRun]
	f.check(t, "POST", retention, admin, cutoff, http.StatusOK, nil)
	f.check(t, "GET", retention, admin, nil, http.StatusOK, &history)
	if len(history.Items) != 1 || history.Items[0].ID != applied.Run.ID {
		t.Fatalf("retention retry did not keep one audit record: %+v", history.Items)
	}
	var synced store.SyncPage
	f.check(t, "GET", base+"/sync?after=0", member.Token, nil, http.StatusOK, &synced)
	assertSyncPage(t, synced, 5, false, 5)
	if synced.EarliestAvailable != 5 || len(synced.Expired) != 2 ||
		synced.Expired[0] != (store.SequenceRange{From: 2, To: 2}) || synced.Expired[1] != (store.SequenceRange{From: 4, To: 4}) {
		t.Fatalf("gap response included an unavailable membership interval: %+v", synced)
	}
	f.check(t, "GET", messages+"/2", member.Token, nil, http.StatusGone, nil)
	f.check(t, "GET", messages+"/3", member.Token, nil, http.StatusForbidden, nil)
	assertReadState(t, f, base+"/read", member.Token, 0, 1)
	var receipt store.MessageReceipt
	f.check(t, "POST", messages, owner.Token, syncTestMessage(2), http.StatusOK, &receipt)
	if !receipt.Duplicate || !receipt.Message.Expired || receipt.Message.Body != "" || receipt.Message.Seq != 2 {
		t.Fatalf("expired retry returned an invalid receipt: %+v", receipt)
	}
	var previous store.Message
	f.check(t, "GET", messages+"/by-client/batch-2", owner.Token, nil, http.StatusOK, &previous)
	if !previous.Expired || previous.Body != "" {
		t.Fatalf("expired message body leaked from original client ID: %+v", previous)
	}
	f.check(t, "POST", messages, owner.Token, map[string]string{"client_id": "batch-2", "body": "Changed payload"}, http.StatusConflict, nil)
}

func syncTestStore(t *testing.T, dsn string) *store.Store {
	t.Helper()
	if _, err := store.Migrate(dsn); err != nil {
		t.Fatal(err)
	}
	storage, err := store.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { storage.Close() })
	return storage
}

func syncTestMessage(seq int) map[string]string {
	return map[string]string{"client_id": fmt.Sprintf("batch-%d", seq), "body": fmt.Sprintf("Message %d", seq)}
}

func assertSyncPage(t *testing.T, result store.SyncPage, after uint64, more bool, seqs ...uint64) {
	t.Helper()
	if len(result.Items) != len(seqs) || result.NextAfter != after || result.HasMore != more {
		t.Fatalf("unexpected sync page: %+v", result)
	}
	for index, seq := range seqs {
		if result.Items[index].Seq != seq {
			t.Fatalf("page item %d has sequence %d, expected %d", index, result.Items[index].Seq, seq)
		}
	}
}

func assertReadState(t *testing.T, f identityFixture, path, token string, seq, unread uint64) {
	t.Helper()
	var actual store.ReadState
	f.check(t, "GET", path, token, nil, http.StatusOK, &actual)
	if actual.Seq != seq || actual.Unread != unread {
		t.Fatalf("read state %+v, expected sequence %d with %d unread", actual, seq, unread)
	}
}

func getFromGateway(t *testing.T, gateway *gatewayFixture, path, token string, result any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, "GET", gateway.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := gateway.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s returned HTTP %d", path, response.StatusCode)
	}
	if err := json.NewDecoder(response.Body).Decode(result); err != nil {
		t.Fatal(err)
	}
}
