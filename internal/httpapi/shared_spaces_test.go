package httpapi

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/axin2024/messaging-cluster/internal/credential"
	"github.com/axin2024/messaging-cluster/internal/store"
)

func TestSharedInvitationsOnMySQL(t *testing.T) {
	dsn := testMySQLDSN(t)
	if _, err := store.Migrate(dsn); err != nil {
		t.Fatal(err)
	}
	storage, err := store.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { storage.Close() })
	dir := t.TempDir()
	server := gatewayTestServerWithMedia(t, storage, "space-primary", attachmentTestStorage(t, dir))
	peerServer := gatewayTestServerWithMedia(t, storage, "space-peer", attachmentTestStorage(t, dir))
	hostTenant, host := messageTestApp(t, storage)
	partner, peer := messageTestApp(t, storage)
	_, outside := messageTestApp(t, storage)
	peerDigest, _ := credential.TokenDigest(peer)
	org, err := storage.Admin(t.Context(), peerDigest)
	if err != nil {
		t.Fatal(err)
	}
	var space store.SharedSpace
	f := identityFixture{handler: server.Config.Handler}
	f.check(t, "POST", "/v1/admin/shared-spaces", host, map[string]string{"key": "采购项目", "title": "采购", "purpose": "合作合同"}, 422, nil)
	f.check(t, "POST", "/v1/admin/shared-spaces", host, map[string]string{"key": "supply", "title": "采购", "purpose": "合作合同"}, 201, &space)
	req := func(handler http.Handler, path, token, key string, data any) *httptest.ResponseRecorder {
		body, _ := json.Marshal(data)
		r := httptest.NewRequest("POST", path, bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	offer := store.Offer{PeerTenant: partner, PeerApp: "dispatch", Scope: []string{"project:read", "thread:write"}, SeatLimit: 5,
		AcceptBefore: time.Now().UTC().Truncate(time.Second).Add(time.Hour), ProjectEndsAt: time.Now().UTC().Truncate(time.Second).Add(24 * time.Hour)}
	invitePath := fmt.Sprintf("/v1/admin/shared-spaces/%d/invitations", space.ID)
	res := req(server.Config.Handler, invitePath, host, "invite", offer)
	var invite store.Invitation
	if res.Code != 201 || json.Unmarshal(res.Body.Bytes(), &invite) != nil || invite.ID == 0 {
		t.Fatal(res.Code, res.Body.String())
	}
	var inbox struct {
		Items []store.IncomingInvitation `json:"items"`
	}
	f.check(t, "GET", "/v1/admin/partner-invitations", peer, nil, 200, &inbox)
	if len(inbox.Items) != 1 || inbox.Items[0].Purpose != space.Purpose || inbox.Items[0].Status != "pending" {
		t.Fatal("incorrect invitation inbox")
	}
	f.check(t, "GET", "/v1/admin/partner-invitations", outside, nil, 200, &inbox)
	if len(inbox.Items) != 0 {
		t.Fatal("foreign invitation leaked")
	}
	acceptPath := fmt.Sprintf("/shared-spaces/%d/partners/%d/accept", space.ID, org.TenantID)
	body := map[string]any{"invitation_id": invite.ID, "policy_rev": invite.PolicyRev, "scope": offer.Scope}
	if r := req(peerServer.Config.Handler, acceptPath, outside, "stolen", body); r.Code != 404 {
		t.Fatal(r.Code, r.Body.String())
	}
	outsideDigest, _ := credential.TokenDigest(outside)
	third, err := storage.Admin(t.Context(), outsideDigest)
	if err != nil {
		t.Fatal(err)
	}
	if r := req(peerServer.Config.Handler, fmt.Sprintf("/shared-spaces/%d/partners/%d/accept", space.ID, third.TenantID), outside, "forwarded", body); r.Code != 404 {
		t.Fatal(r.Code, r.Body.String())
	}
	results := make(chan *httptest.ResponseRecorder, 2)
	for _, instance := range []http.Handler{server.Config.Handler, peerServer.Config.Handler} {
		go func() { results <- req(instance, acceptPath, peer, "confirm", body) }()
	}
	var first store.PartnerLink
	for range 2 {
		r := <-results
		var link store.PartnerLink
		if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &link) != nil || link.ID == 0 || link.GrantEpoch != 1 {
			t.Fatal(r.Code, r.Body.String())
		}
		if first.ID != 0 && !reflect.DeepEqual(first, link) {
			t.Fatal("different links for one consent")
		}
		first = link
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err = db.QueryRow("SELECT COUNT(*) FROM partner_events WHERE link_id=? AND action='accepted'", first.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("acceptance events %d, %v", n, err)
	}
	body["scope"] = []string{"project:read"}
	if r := req(server.Config.Handler, acceptPath, peer, "confirm", body); r.Code != 409 {
		t.Fatal(r.Code, r.Body.String())
	}
	grants := fmt.Sprintf("/v1/admin/partner-links/%d/grants", first.ID)
	other := identityFixture{handler: peerServer.Config.Handler}
	owner := messageTestUser(t, f, hostTenant, host, "buyer")
	ownerGrant := store.GrantChange{UserID: owner.Session.UserID, Role: "buyer", ExpiresAt: offer.AcceptBefore}
	if r := req(server.Config.Handler, grants, host, "buyer", ownerGrant); r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	channelPath := fmt.Sprintf("/v1/admin/partner-links/%d/channels", first.ID)
	var shared, private store.SharedChannel
	input := map[string]string{"key": "supply", "title": "Supply", "audience": "shared", "history_from": "beginning"}
	f.check(t, "POST", channelPath, host, input, 409, nil)
	input["history_from"] = "join"
	f.check(t, "POST", channelPath, peer, input, 404, nil)
	f.check(t, "POST", channelPath, host, input, 201, &shared)
	input["key"], input["audience"], input["history_from"] = "internal", "host", "beginning"
	f.check(t, "POST", channelPath, host, input, 201, &private)
	sharedPath := fmt.Sprintf("/v1/shared-channels/%d/messages", shared.ID)
	privatePath := fmt.Sprintf("/v1/shared-channels/%d/messages", private.ID)
	const olderContent = "negotiation before the supplier joined"
	oldFile := createTestAttachment(t, f, owner.Token, "old.txt", olderContent)
	checkUpload(t, server, fmt.Sprintf("/v1/attachments/%d/content", oldFile.ID), owner.Token, olderContent, 200)
	f.check(t, "POST", sharedPath, owner.Token,
		store.SharedMessageInput{ClientID: "older", Body: "before the supplier joined", Attachments: []uint64{oldFile.ID}}, 201, nil)
	f.check(t, "POST", privatePath, owner.Token, store.SharedMessageInput{ClientID: "internal", Body: "internal supplier calculations"}, 201, nil)
	changes := map[uint64]store.GrantChange{}
	users := map[uint64]sessionResponse{}
	for i := range 6 {
		user := messageTestUser(t, f, partner, peer, fmt.Sprintf("seat-%d", i))
		changes[user.Session.UserID] = store.GrantChange{UserID: user.Session.UserID, Role: "dispatch", ExpiresAt: offer.AcceptBefore}
		users[user.Session.UserID] = user
	}
	barrier := make(chan struct{})
	seatResults := make(chan *httptest.ResponseRecorder, len(changes))
	for user, change := range changes {
		go func() {
			<-barrier
			seatResults <- req(server.Config.Handler, grants, peer, fmt.Sprintf("seat-%d", user), change)
		}()
	}
	close(barrier)
	var granted []store.GrantReceipt
	for range changes {
		r := <-seatResults
		if r.Code == 409 {
			continue
		}
		var g store.GrantReceipt
		if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &g) != nil {
			t.Fatal(r.Code, r.Body.String())
		}
		granted = append(granted, g)
	}
	if len(granted) != 5 {
		t.Fatalf("accepted %d seats", len(granted))
	}
	example := granted[0]
	key := fmt.Sprintf("seat-%d", example.UserID)
	var replay store.GrantReceipt
	r := req(peerServer.Config.Handler, grants, peer, key, changes[example.UserID])
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &replay) != nil || !reflect.DeepEqual(replay, example) {
		t.Fatal("lost original seat receipt")
	}
	supplier := users[example.UserID]
	var channels struct {
		Items []store.SharedChannel `json:"items"`
	}
	other.check(t, "GET", fmt.Sprintf("/v1/partner-links/%d/channels", first.ID), supplier.Token, nil, 200, &channels)
	if !reflect.DeepEqual(channels.Items, []store.SharedChannel{shared}) {
		t.Fatalf("supplier channel list: %+v", channels.Items)
	}
	other.check(t, "GET", privatePath, supplier.Token, nil, 404, nil)
	oldGrant := fmt.Sprintf("%s/1/attachments/%d/grant", sharedPath, oldFile.ID)
	other.check(t, "POST", oldGrant, supplier.Token, nil, 404, nil)
	oldTicket := issueTestGrant(t, f, oldGrant, owner.Token)
	const recentContent = "shared order specification"
	recentFile := createTestAttachment(t, f, owner.Token, "specification.txt", recentContent)
	checkUpload(t, server, fmt.Sprintf("/v1/attachments/%d/content", recentFile.ID), owner.Token, recentContent, 200)
	for i := 1; i <= 3; i++ {
		item := store.SharedMessageInput{ClientID: fmt.Sprintf("update-%d", i), Body: fmt.Sprintf("supplier update %d", i)}
		if i == 1 {
			item.Attachments = []uint64{recentFile.ID}
		}
		f.check(t, "POST", sharedPath, owner.Token, item, 201, nil)
	}
	var timeline store.SharedPage
	other.check(t, "GET", sharedPath, supplier.Token, nil, 200, &timeline)
	if len(timeline.Items) != 3 || timeline.Next == "" || timeline.Items[0].Seq != 2 || timeline.Items[2].Seq != 4 {
		t.Fatalf("unexpected post-join history: %+v", timeline)
	}
	if len(timeline.Items[0].Attachments) != 1 || timeline.Items[0].Attachments[0].ID != recentFile.ID ||
		timeline.Items[0].Attachments[0].SHA256 != recentFile.SHA256 || timeline.Items[0].Attachments[0].ObjectKey != nil {
		t.Fatalf("unexpected shared file metadata: %+v", timeline.Items[0].Attachments)
	}
	grantPath := fmt.Sprintf("%s/2/attachments/%d/grant", sharedPath, recentFile.ID)
	validTicket := issueTestGrant(t, f, grantPath, supplier.Token)
	unusedTicket := issueTestGrant(t, other, grantPath, supplier.Token)
	download := func(session, ticket string, status int, value string) {
		t.Helper()
		r, err := http.NewRequestWithContext(t.Context(), "GET", peerServer.URL+"/v1/shared-attachment-content", nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+session)
		r.Header.Set("X-Attachment-Token", ticket)
		resp, err := peerServer.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != status || resp.Header.Get("Cache-Control") != "no-store" ||
			(status == 200 && string(b) != value) || (status != 200 && string(b) != "{\"error\":\"not_found\"}\n") {
			t.Fatalf("shared download: status=%d body=%q error=%v", resp.StatusCode, b, err)
		}
	}
	download(supplier.Token, oldTicket, 404, "")
	download(supplier.Token, validTicket, 200, recentContent)
	if err = db.QueryRow(`SELECT COUNT(*) FROM shared_attachment_access WHERE link_id=? AND channel_id=? AND seq=2 AND
		attachment_id=? AND recipient_tenant=? AND recipient_user=? AND device_id=? AND grant_epoch=?`,
		first.ID, shared.ID, recentFile.ID, supplier.Session.TenantID, supplier.Session.UserID, supplier.Session.DeviceID, timeline.Epoch).Scan(&n); err != nil || n != 1 {
		t.Fatalf("shared download receipts %d, %v", n, err)
	}
	oldCursor := sharedPath + "?cursor=" + timeline.Next
	phone := messageTestLogin(t, f, partner, supplier.Session.Username, "phone")
	f.check(t, "GET", oldCursor, phone.Token, nil, 409, nil)
	f.check(t, "GET", sharedPath, phone.Token, nil, 200, nil)
	download(phone.Token, validTicket, 404, "")
	root, older := uint64(2), uint64(1)
	other.check(t, "POST", sharedPath, supplier.Token, store.SharedMessageInput{ClientID: "inaccessible", Body: "hidden quote", Quote: &older}, 404, nil)
	other.check(t, "POST", sharedPath, owner.Token,
		store.SharedMessageInput{ClientID: "cross-channel", Body: "private quote", Quote: &older, QuoteChannel: &private.ID}, 404, nil)
	for i := 1; i <= 3; i++ {
		item := store.SharedMessageInput{ClientID: fmt.Sprintf("reply-%d", i), Body: fmt.Sprintf("reply %d", i), Root: &root}
		other.check(t, "POST", sharedPath, supplier.Token, item, 201, nil)
	}
	thread := fmt.Sprintf("/v1/shared-channels/%d/threads/%d", shared.ID, root)
	f.check(t, "GET", thread+"?limit=2", owner.Token, nil, 200, &timeline)
	if len(timeline.Items) != 2 || !timeline.More || timeline.Items[0].Seq != root || timeline.Items[1].Body != "reply 1" {
		t.Fatalf("unexpected thread start: %+v", timeline)
	}
	f.check(t, "GET", sharedPath+"?cursor="+timeline.Next, owner.Token, nil, 409, nil)
	other.check(t, "GET", thread+"?cursor="+timeline.Next, owner.Token, nil, 200, &timeline)
	if len(timeline.Items) != 2 || timeline.More || timeline.Items[0].Body != "reply 2" || timeline.Items[1].Body != "reply 3" {
		t.Fatalf("unexpected thread continuation: %+v", timeline)
	}
	searchPath := fmt.Sprintf("/v1/partner-links/%d/search", first.ID)
	var matches store.SharedSearchPage
	other.check(t, "GET", searchPath+"?q=supplier&limit=1", supplier.Token, nil, 200, &matches)
	if len(matches.Items) != 1 || matches.Items[0].ChannelID != shared.ID || matches.Items[0].Seq != 2 || !matches.HasMore || matches.PolicyRev != 1 {
		t.Fatalf("supplier search first page: %+v", matches)
	}
	other.check(t, "GET", searchPath+"?q=reply&cursor="+matches.Cursor, supplier.Token, nil, 409, nil)
	f.check(t, "GET", searchPath+"?q=supplier&cursor="+matches.Cursor, supplier.Token, nil, 200, &matches)
	if len(matches.Items) != 2 || matches.Items[0].Seq != 3 || matches.Items[1].Seq != 4 || matches.HasMore {
		t.Fatalf("supplier search continuation: %+v", matches)
	}
	oldSearch := searchPath + "?q=supplier&cursor=" + matches.Cursor
	f.check(t, "GET", searchPath+"?q=supplier", owner.Token, nil, 200, &matches)
	if len(matches.Items) != 5 || matches.Items[1].ChannelID != private.ID {
		t.Fatalf("host search across channels: %+v", matches)
	}
	hidePath := fmt.Sprintf("%s/%d/messages/4", channelPath, shared.ID)
	other.check(t, "DELETE", hidePath, peer, nil, 404, nil)
	f.check(t, "DELETE", hidePath, host, nil, 204, nil)
	other.check(t, "DELETE", hidePath, host, nil, 204, nil)
	// Hold a stale projection in place independently of the cleanup ticker.
	_, err = db.Exec(`INSERT INTO shared_search_index (link_id,project_seq,channel_id,seq,root_seq,body,purge_after)
		SELECT ?,project_seq,channel_id,seq,root_seq,'supplier update 3',TIMESTAMPADD(HOUR,1,UTC_TIMESTAMP(6))
		FROM shared_messages WHERE channel_id=? AND seq=4
		ON DUPLICATE KEY UPDATE purge_after=TIMESTAMPADD(HOUR,1,UTC_TIMESTAMP(6))`, first.ID, shared.ID)
	if err != nil {
		t.Fatal(err)
	}
	other.check(t, "GET", searchPath+"?q=supplier", supplier.Token, nil, 200, &matches)
	if len(matches.Items) != 2 || matches.Items[1].Seq != 3 {
		t.Fatalf("search leaked hidden message with stale index: %+v", matches)
	}
	if err = db.QueryRow(`SELECT COUNT(*) FROM shared_message_hides WHERE channel_id=? AND seq=4`, shared.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("hide audit receipts %d, %v", n, err)
	}
	if _, err = db.Exec(`UPDATE shared_search_index SET purge_after=UTC_TIMESTAMP(6) WHERE channel_id=? AND seq=4`, shared.ID); err != nil {
		t.Fatal(err)
	}
	exports := fmt.Sprintf("/v1/admin/partner-links/%d/exports", first.ID)
	var archive store.SharedExport
	r = req(server.Config.Handler, exports, host, "archive-1", store.SharedExportInput{})
	if r.Code != 202 || json.Unmarshal(r.Body.Bytes(), &archive) != nil || archive.ID == 0 || archive.State != "queued" {
		t.Fatal(r.Code, r.Body.String())
	}
	archivePath := fmt.Sprintf("%s/%d", exports, archive.ID)
	if r.Header().Get("Location") != archivePath {
		t.Fatal("missing export receipt link")
	}
	r = req(peerServer.Config.Handler, exports, host, "archive-1", store.SharedExportInput{})
	var priorArchive store.SharedExport
	if (r.Code != 202 && r.Code != 200) || json.Unmarshal(r.Body.Bytes(), &priorArchive) != nil || priorArchive.ID != archive.ID {
		t.Fatal("export replay changed operation ID")
	}
	if r = req(server.Config.Handler, exports, host, "archive-1", store.SharedExportInput{After: 1}); r.Code != 409 {
		t.Fatal(r.Code, r.Body.String())
	}
	if r = req(server.Config.Handler, exports, host, "bad-range", store.SharedExportInput{After: 1000}); r.Code != 422 {
		t.Fatal(r.Code, r.Body.String())
	}
	other.check(t, "GET", archivePath, peer, nil, 404, nil)
	deadline := time.Now().Add(10 * time.Second)
	for {
		other.check(t, "GET", archivePath, host, nil, 200, &archive)
		if archive.State == "ready" {
			break
		}
		if archive.State != "queued" || time.Now().After(deadline) {
			t.Fatalf("export worker did not finish snapshot: %+v", archive)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if archive.Items != 4 || archive.HasMore || archive.SHA256 == "" || archive.Attempts != 1 {
		t.Fatalf("unexpected export result: %+v", archive)
	}
	var exportGrant struct {
		Token     string `json:"token"`
		ExpiresIn int    `json:"expires_in"`
	}
	f.check(t, "POST", archivePath+"/grant", host, nil, 201, &exportGrant)
	if exportGrant.ExpiresIn != 300 {
		t.Fatal("unexpected export grant lifetime")
	}
	exportDownload := func(admin string, status int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest("GET", archivePath+"/content", nil)
		r.Header.Set("Authorization", "Bearer "+admin)
		r.Header.Set("X-Export-Token", exportGrant.Token)
		w := httptest.NewRecorder()
		peerServer.Config.Handler.ServeHTTP(w, r)
		if w.Code != status || (status != 200 && w.Body.String() != "{\"error\":\"not_found\"}\n") {
			t.Fatalf("export download got %d: %s", w.Code, w.Body.String())
		}
		return w
	}
	exportDownload(peer, 404)
	archived := exportDownload(host, 200)
	if fmt.Sprintf("%x", sha256.Sum256(archived.Body.Bytes())) != archive.SHA256 || archived.Header().Get("X-Archive-SHA256") != archive.SHA256 ||
		!bytes.Contains(archived.Body.Bytes(), []byte(recentFile.SHA256)) || !bytes.Contains(archived.Body.Bytes(), []byte("internal supplier calculations")) ||
		bytes.Contains(archived.Body.Bytes(), []byte("reply 1")) || bytes.Contains(archived.Body.Bytes(), []byte("supplier update 3")) {
		t.Fatal("export bytes did not match digest, authorization, and snapshot")
	}
	revoke := store.GrantChange{UserID: example.UserID, ExpectedVersion: example.Version, Revoke: true}
	if r = req(peerServer.Config.Handler, grants, outside, "revoke", revoke); r.Code != 404 {
		t.Fatal(r.Code, r.Body.String())
	}
	r = req(peerServer.Config.Handler, grants, host, "emergency", revoke)
	var revoked store.GrantReceipt
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &revoked) != nil || !revoked.Revoked || revoked.ActiveSeats != 4 {
		t.Fatal(r.Code, r.Body.String())
	}
	if len(revoked.Devices) != 2 || revoked.MoreDevices || revoked.Devices[0].DeviceID == revoked.Devices[1].DeviceID {
		t.Fatalf("missing prior device authorization readback: %+v", revoked.Devices)
	}
	for _, device := range revoked.Devices {
		if device.GrantEpoch+1 != revoked.Epoch || device.GrantVersion != example.Version || device.AuthorizedAt.IsZero() {
			t.Fatalf("misstated device authorization: %+v", device)
		}
	}
	other.check(t, "GET", oldCursor, supplier.Token, nil, 404, nil)
	f.check(t, "GET", sharedPath, phone.Token, nil, 404, nil)
	other.check(t, "GET", oldSearch, supplier.Token, nil, 404, nil)
	f.check(t, "GET", searchPath+"?q=supplier", supplier.Token, nil, 404, nil)
	download(supplier.Token, unusedTicket, 404, "")
	other.check(t, "POST", grantPath, supplier.Token, nil, 404, nil)
	exportDownload(host, 404)
	other.check(t, "POST", archivePath+"/grant", host, nil, 404, nil)
	f.check(t, "GET", archivePath, host, nil, 200, &archive)
	if archive.State != "invalidated" {
		t.Fatalf("old export still valid after grant epoch advanced: %+v", archive)
	}
	if err = db.QueryRow(`SELECT COUNT(*) FROM shared_search_index WHERE channel_id=? AND seq=2`, shared.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("revoke scenario has no stale search content, count=%d error=%v", n, err)
	}
	deadline = time.Now().Add(10 * time.Second)
	for {
		if err = db.QueryRow(`SELECT COUNT(*) FROM shared_search_index WHERE channel_id=? AND seq=4`, shared.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("search worker did not clean up due projection within 10 seconds")
		}
		time.Sleep(50 * time.Millisecond)
	}
	regrant := changes[example.UserID]
	regrant.ExpectedVersion = revoked.Version
	r = req(peerServer.Config.Handler, grants, peer, "regrant", regrant)
	var restored store.GrantReceipt
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &restored) != nil || restored.Version != revoked.Version+1 {
		t.Fatal(r.Code, r.Body.String())
	}
	f.check(t, "GET", oldCursor, supplier.Token, nil, 409, nil)
	other.check(t, "GET", sharedPath, supplier.Token, nil, 200, &timeline)
	if len(timeline.Items) != 0 || timeline.Epoch != restored.Epoch {
		t.Fatalf("rejoin returned old messages: %+v", timeline)
	}
	statePath := fmt.Sprintf("/v1/admin/partner-links/%d/state", first.ID)
	transition := func(admin, state string, epoch uint64) store.PartnerState {
		t.Helper()
		var result store.PartnerState
		other.check(t, "PUT", statePath, admin, map[string]any{"grant_epoch": epoch, "state": state}, 200, &result)
		return result
	}
	hostFrozen := transition(host, "frozen", restored.Epoch)
	peerFrozen := transition(peer, "frozen", hostFrozen.Epoch)
	hostResumed := transition(host, "active", peerFrozen.Epoch)
	if hostResumed.State != "frozen" || hostResumed.HostFrozen || !hostResumed.PeerFrozen {
		t.Fatalf("host cleared peer freeze: %+v", hostResumed)
	}
	f.check(t, "POST", sharedPath, owner.Token, store.SharedMessageInput{ClientID: "frozen", Body: "cannot post while frozen"}, 403, nil)
	resumed := transition(peer, "active", hostResumed.Epoch)
	if resumed.State != "active" || resumed.HostFrozen || resumed.PeerFrozen {
		t.Fatalf("both sides resumed: %+v", resumed)
	}
	state := map[string]any{"grant_epoch": resumed.Epoch, "state": "closed"}
	f.check(t, "PUT", statePath, outside, state, 404, nil)
	f.check(t, "PUT", statePath, peer, state, 200, nil)
	f.check(t, "PUT", statePath, peer, state, 200, nil)
	f.check(t, "PUT", statePath, host, map[string]any{"grant_epoch": 2, "state": "active"}, 409, nil)
	other.check(t, "GET", sharedPath, owner.Token, nil, 404, nil)
	auditPath := fmt.Sprintf("/v1/admin/partner-links/%d/audit", first.ID)
	var audit struct {
		Items []store.SharedAuditEvent `json:"items"`
	}
	other.check(t, "GET", auditPath, outside, nil, 404, nil)
	other.check(t, "GET", auditPath, host, nil, 200, &audit)
	var ownGrant, closure bool
	for _, entry := range audit.Items {
		if entry.Action == "grant_set" {
			if entry.Actor == org || !bytes.Contains(entry.Detail, []byte(`"buyer"`)) {
				t.Fatal("host audit leaked peer grant or hid own grant")
			}
			ownGrant = true
		}
		if entry.Action == "closed" {
			closure = entry.Epoch == resumed.Epoch+1 && entry.Actor == org
		}
	}
	if !ownGrant || !closure {
		t.Fatal("host lost own grant or closure evidence after peer exit")
	}
	f.check(t, "GET", auditPath, peer, nil, 200, &audit)
	var downloadAudit bool
	for _, entry := range audit.Items {
		if entry.Action == "attachment_download_authorized" {
			downloadAudit = entry.Actor == org
		}
		if entry.Action == "grant_revoked" || entry.Action == "message_hidden" {
			t.Fatalf("supplier audit leaked host event: %+v", entry)
		}
	}
	if !downloadAudit {
		t.Fatal("supplier lost own download audit after exit")
	}
	res = req(server.Config.Handler, invitePath, host, "expire", offer)
	if res.Code != 201 || json.Unmarshal(res.Body.Bytes(), &invite) != nil {
		t.Fatal(res.Code, res.Body.String())
	}
	if _, err = db.Exec("UPDATE partner_invitations SET accept_before=TIMESTAMPADD(SECOND,-1,UTC_TIMESTAMP(6)) WHERE id=?", invite.ID); err != nil {
		t.Fatal(err)
	}
	body["invitation_id"] = invite.ID
	if r := req(peerServer.Config.Handler, acceptPath, peer, "late", body); r.Code != 410 {
		t.Fatal(r.Code, r.Body.String())
	}
}
