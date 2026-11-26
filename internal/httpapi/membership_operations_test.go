package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/axin2024/messaging-cluster/internal/store"
)

func TestMembershipOperationsOnMySQL(t *testing.T) {
	dsn := testMySQLDSN(t)
	if _, err := store.Migrate(dsn); err != nil {
		t.Fatal(err)
	}
	storage, err := store.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { storage.Close() })
	server := gatewayTestServer(t, storage, "membership")
	second := gatewayTestServer(t, storage, "membership-second")
	f := identityFixture{handler: server.Config.Handler}
	otherGateway := identityFixture{handler: second.Config.Handler}
	tenant, admin := messageTestApp(t, storage)
	owner := messageTestUser(t, f, tenant, admin, "owner")
	manager := messageTestUser(t, f, tenant, admin, "manager")
	phone := messageTestLogin(t, f, tenant, "manager", "phone")
	member := messageTestUser(t, f, tenant, admin, "member")
	newMember := messageTestUser(t, f, tenant, admin, "new-member")
	otherTenant, otherAdmin := messageTestApp(t, storage)
	outsider := messageTestUser(t, f, otherTenant, otherAdmin, "outsider")

	var group store.Conversation
	f.check(t, "POST", "/v1/conversations/group", owner.Token, map[string]string{
		"title": "Dispatch roster", "history_policy": "since_join",
	}, http.StatusCreated, &group)
	base := fmt.Sprintf("/v1/conversations/%d", group.ID)
	role := func(user uint64, name string) {
		f.check(t, "PUT", fmt.Sprintf("%s/members/%d", base, user), owner.Token,
			map[string]string{"role": name}, http.StatusOK, nil)
	}
	role(manager.Session.UserID, "admin")
	role(member.Session.UserID, "member")
	batch := base + "/membership-batches"
	audit := base + "/membership-operations"
	items := []store.MembershipChange{
		{UserID: newMember.Session.UserID, Action: "set", Role: "member"},
		{UserID: outsider.Session.UserID, Action: "set", Role: "member"},
		{UserID: owner.Session.UserID, Action: "remove"},
		{UserID: manager.Session.UserID, Action: "remove"},
		{UserID: member.Session.UserID, Action: "remove"},
	}
	payload := func(clientID string, changes []store.MembershipChange) map[string]any {
		return map[string]any{"client_id": clientID, "items": changes}
	}
	f.check(t, "POST", batch, member.Token, payload("denied", items), http.StatusForbidden, nil)
	f.check(t, "GET", audit, member.Token, nil, http.StatusNotFound, nil)
	f.check(t, "GET", audit, outsider.Token, nil, http.StatusNotFound, nil)
	f.check(t, "POST", batch, manager.Token, payload("duplicates", []store.MembershipChange{items[0], items[0]}), http.StatusBadRequest, nil)
	var prepared, fetched store.MembershipOperation
	f.check(t, "POST", batch, manager.Token, payload("roster-12", items), http.StatusOK, &prepared)
	otherGateway.check(t, "GET", batch+"/roster-12", phone.Token, nil, http.StatusOK, &fetched)
	if prepared.ID == 0 || prepared.Version != 1 || prepared.ConversationID != group.ID || prepared.ActorID != manager.Session.UserID ||
		prepared.DeviceID != manager.Session.DeviceID || prepared.ActorRole != "admin" || !reflect.DeepEqual(prepared, fetched) {
		t.Fatalf("incorrect prepared receipt: %+v, fetched %+v", prepared, fetched)
	}
	for index, item := range prepared.Items {
		if item.Status != "not_started" || item.MembershipChange != items[index] {
			t.Fatalf("incorrect planned change %d: %+v", index, item)
		}
	}
	f.check(t, "GET", base+"/members", newMember.Token, nil, http.StatusNotFound, nil)
	f.check(t, "GET", batch+"/roster-12", owner.Token, nil, http.StatusNotFound, nil)
	f.check(t, "POST", batch+"/roster-12/execute", owner.Token, nil, http.StatusNotFound, nil)
	f.check(t, "POST", batch, phone.Token, payload("roster-12", items[1:]), http.StatusConflict, nil)

	started := make(chan struct{})
	responses := make(chan *httptest.ResponseRecorder, 2)
	for _, gateway := range []identityFixture{f, otherGateway} {
		go func() {
			<-started
			responses <- gateway.request("POST", batch+"/roster-12/execute", phone.Token, nil)
		}()
	}
	close(started)
	var executed store.MembershipOperation
	for range 2 {
		response := <-responses
		var receipt store.MembershipOperation
		if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil || response.Code != http.StatusOK {
			t.Fatalf("execute: HTTP %d %s", response.Code, response.Body)
		}
		if executed.ID != 0 && !reflect.DeepEqual(executed, receipt) {
			t.Fatalf("concurrent execution did not replay the same receipt: %+v and %+v", executed, receipt)
		}
		executed = receipt
	}
	if executed.ID != prepared.ID || executed.Version != 2 || executed.CompletedAt == nil || executed.DeviceID != phone.Session.DeviceID {
		t.Fatalf("incorrect execution receipt: %+v", executed)
	}
	statuses := []string{"succeeded", "failed", "failed", "failed", "succeeded"}
	reasons := []string{"", "not_found", "forbidden", "forbidden", ""}
	for index, item := range executed.Items {
		if item.Status != statuses[index] || item.Reason != reasons[index] || item.MembershipChange != items[index] {
			t.Fatalf("incorrect outcome %d: %+v", index, item)
		}
	}
	f.check(t, "GET", base+"/members", newMember.Token, nil, http.StatusOK, nil)
	f.check(t, "GET", base+"/members", member.Token, nil, http.StatusNotFound, nil)
	f.check(t, "POST", batch, phone.Token, payload("roster-12", items), http.StatusOK, &fetched)
	if !reflect.DeepEqual(executed, fetched) {
		t.Fatalf("completed batch did not survive prepare replay: %+v", fetched)
	}

	// The owner can transfer control and the former owner's later admin action is still attributed to them.
	f.check(t, "PUT", base+"/owner", owner.Token, map[string]uint64{"user_id": newMember.Session.UserID}, http.StatusNoContent, nil)
	f.check(t, "DELETE", fmt.Sprintf("%s/members/%d", base, owner.Session.UserID), owner.Token, nil, http.StatusNoContent, nil)
	f.check(t, "GET", audit, owner.Token, nil, http.StatusNotFound, nil)
	var log page[store.MembershipOperation]
	f.check(t, "GET", audit, newMember.Token, nil, http.StatusOK, &log)
	if len(log.Items) != 5 || log.Items[0].Kind != "remove" || log.Items[0].ActorID != owner.Session.UserID || log.Items[0].ActorRole != "admin" ||
		log.Items[1].Kind != "transfer" || log.Items[1].ActorID != owner.Session.UserID || log.Items[1].ActorRole != "owner" ||
		!reflect.DeepEqual(log.Items[2], executed) || log.Items[3].Kind != "set" || log.Items[4].Kind != "set" {
		t.Fatalf("incorrect operation audit: %+v", log.Items)
	}
	f.check(t, "GET", fmt.Sprintf("%s?before=%d", audit, log.Items[2].ID), newMember.Token, nil, http.StatusOK, &log)
	if len(log.Items) != 2 {
		t.Fatalf("incorrect audit cursor length: %d", len(log.Items))
	}
}
