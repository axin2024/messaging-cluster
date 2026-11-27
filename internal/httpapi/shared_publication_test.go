package httpapi

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/axin2024/messaging-cluster/internal/store"
)

func TestSharedPublicationAndMentionsOnMySQL(t *testing.T) {
	dsn := testMySQLDSN(t)
	storage := syncTestStore(t, dsn)
	server := gatewayTestServer(t, storage, "space-publications")
	f := identityFixture{handler: server.Config.Handler}
	hostTenant, host := messageTestApp(t, storage)
	peerTenant, peer := messageTestApp(t, storage)
	owner := messageTestUser(t, f, hostTenant, host, "owner")
	colleague := messageTestUser(t, f, hostTenant, host, "colleague")
	reader := messageTestUser(t, f, peerTenant, peer, "supplier")
	ctx := t.Context()
	space, err := storage.CreateSpace(ctx, owner.Session.Scope, "publication", "采购协作", "共享选定的采购资料")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	permissions := []string{"project:read", "thread:write"}
	offer := store.Offer{PeerTenant: peerTenant, PeerApp: "dispatch", Scope: permissions, SeatLimit: 5, RetentionDays: 1, AcceptBefore: now.Add(time.Hour), ProjectEndsAt: now.Add(2 * time.Hour)}
	invite, err := storage.InvitePartner(ctx, owner.Session.Scope, space.ID, "invite", offer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = storage.AcceptPartner(ctx, reader.Session.Scope, space.ID, invite.ID, invite.PolicyRev, "confirm", permissions, false, 2); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("different retention was accepted: %v", err)
	}
	link, err := storage.AcceptPartner(ctx, reader.Session.Scope, space.ID, invite.ID, invite.PolicyRev, "confirm", permissions, false, invite.RetentionDays)
	if err != nil {
		t.Fatal(err)
	}
	grant := func(user store.Session) store.GrantReceipt {
		t.Helper()
		g, err := storage.ChangeSharedGrant(ctx, user.Scope, link.ID, "grant-"+strconv.FormatUint(user.UserID, 10),
			store.GrantChange{UserID: user.UserID, Role: "member", ExpiresAt: offer.ProjectEndsAt})
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	grant(owner.Session)
	grant(colleague.Session)
	var channel, private store.SharedChannel
	management := fmt.Sprintf("/v1/admin/partner-links/%d", link.ID)
	f.check(t, "POST", management+"/channels", host, map[string]string{"key": "external", "title": "交付", "audience": "shared", "history_from": "join"}, 201, &channel)
	f.check(t, "POST", management+"/channels", host, map[string]string{"key": "private", "title": "内审", "audience": "host", "history_from": "join"}, 201, &private)
	root := fmt.Sprintf("/v1/shared-channels/%d", channel.ID)
	early, current := uint64(1), uint64(2)
	f.check(t, "POST", root+"/messages", owner.Token, store.SharedMessageInput{ClientID: "before", Body: "early thread"}, 201, nil)
	partnerGrant := grant(reader.Session)
	f.check(t, "POST", root+"/messages", owner.Token, store.SharedMessageInput{ClientID: "visible", Body: "工单在交付后需要核对"}, 201, nil)
	var candidates struct {
		Items []store.MentionCandidate `json:"items"`
	}
	f.check(t, "GET", root+"/mention-candidates?side=partner&root_seq=1", owner.Token, nil, 200, &candidates)
	if len(candidates.Items) != 0 {
		t.Fatalf("late member selected for historic thread: %+v", candidates)
	}
	f.check(t, "GET", root+"/mention-candidates?side=partner&root_seq=2&duty_role=dispatch", owner.Token, nil, 200, &candidates)
	if len(candidates.Items) != 0 {
		t.Fatalf("unexpected partner duty: %+v", candidates)
	}
	f.check(t, "GET", root+"/mention-candidates?side=partner&root_seq=2&duty_role=member", owner.Token, nil, 200, &candidates)
	if len(candidates.Items) != 1 || candidates.Items[0].UserID != reader.Session.UserID || candidates.Items[0].Role != "member" {
		t.Fatalf("wrong partner audience: %+v", candidates)
	}
	input := store.SharedMessageInput{ClientID: "mention", Body: "请确认交付日", Root: &early, Mentions: []uint64{reader.Session.UserID}}
	f.check(t, "POST", root+"/messages", owner.Token, input, 422, nil)
	input.Root = nil
	f.check(t, "POST", fmt.Sprintf("/v1/shared-channels/%d/messages", private.ID), owner.Token, input, 422, nil)
	input.Root = &current
	var msg map[string]uint64
	f.check(t, "POST", root+"/messages", owner.Token, input, 201, &msg)
	if msg["seq"] != 3 {
		t.Fatal("rejected mentions consumed a sequence")
	}
	var notifications struct {
		Items []store.SharedMention `json:"items"`
	}
	f.check(t, "GET", root+"/mentions", reader.Token, nil, 200, &notifications)
	if len(notifications.Items) != 1 || notifications.Items[0].Seq != 3 || notifications.Items[0].Root == nil || *notifications.Items[0].Root != current {
		t.Fatalf("wrong recipient readback: %+v", notifications)
	}
	var group store.Conversation
	f.check(t, "POST", "/v1/conversations/group", owner.Token, map[string]string{"title": "普通群", "history_policy": "since_join"}, 201, &group)
	groupPath := fmt.Sprintf("/v1/conversations/%d", group.ID)
	f.check(t, "PUT", fmt.Sprintf("%s/members/%d", groupPath, colleague.Session.UserID), owner.Token, map[string]string{"role": "member"}, 200, nil)
	f.check(t, "POST", groupPath+"/messages", owner.Token, store.OutgoingMessage{ClientID: "original", Body: "采购清单仅发布此条"}, 201, nil)
	preview := fmt.Sprintf("%s/publication-preview?group_id=%d&source_seq=1", root, group.ID)
	f.check(t, "GET", preview, colleague.Token, nil, 403, nil)
	f.check(t, "GET", preview, reader.Token, nil, 403, nil)
	var text map[string]string
	f.check(t, "GET", preview, owner.Token, nil, 200, &text)
	if text["body"] != "采购清单仅发布此条" {
		t.Fatalf("wrong legacy preview: %+v", text)
	}
	publish := store.LegacyPublication{ClientID: "publish-1", GroupID: group.ID, Seq: 1}
	for range 2 {
		f.check(t, "POST", root+"/publications", owner.Token, publish, 201, &msg)
		if msg["seq"] != 4 {
			t.Fatal("legacy publication was duplicated")
		}
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var originCount int
	if err = db.QueryRow(`SELECT COUNT(*) FROM shared_message_origins WHERE channel_id=? AND seq=4 AND source_conversation=? AND source_seq=1 AND actor_user=?`,
		channel.ID, group.ID, owner.Session.UserID).Scan(&originCount); err != nil || originCount != 1 {
		t.Fatalf("original message trace: %d, %v", originCount, err)
	}
	var projects struct {
		Items []store.ProjectLink `json:"items"`
	}
	f.check(t, "GET", "/v1/shared-projects", reader.Token, nil, 200, &projects)
	if len(projects.Items) != 1 || projects.Items[0].MySide != "partner" || projects.Items[0].ID != link.ID {
		t.Fatalf("wrong project listing: %+v", projects)
	}
	var matches store.SharedSearchPage
	f.check(t, "GET", fmt.Sprintf("/v1/partner-links/%d/search?q=%s", link.ID, url.QueryEscape("采购清单")), reader.Token, nil, 200, &matches)
	if len(matches.Items) != 1 || matches.Items[0].Seq != 4 {
		t.Fatalf("search after legacy publication: %+v", matches)
	}
	var previewHost, previewPeer store.SharedExportPreview
	f.check(t, "GET", management+"/export-preview", host, nil, 200, &previewHost)
	f.check(t, "GET", management+"/export-preview", peer, nil, 200, &previewPeer)
	if previewHost.Items != 4 || previewPeer.Items != 0 || previewHost.Snapshot != previewPeer.Snapshot {
		t.Fatalf("export preview counts host=%+v peer=%+v", previewHost, previewPeer)
	}
	var deadlineDiffSeconds int
	if err = db.QueryRow(`SELECT TIMESTAMPDIFF(SECOND,created_at,retained_until) FROM shared_messages WHERE channel_id=? AND seq=2`,
		channel.ID).Scan(&deadlineDiffSeconds); err != nil || deadlineDiffSeconds != 86400 {
		t.Fatalf("agreed retention was not recorded: %d %v", deadlineDiffSeconds, err)
	}
	// Advance the root's deadline in the authoritative row while the search projection and reply still exist.
	if _, err = db.Exec(`UPDATE shared_messages SET retained_until=UTC_TIMESTAMP(6) WHERE channel_id=? AND seq=2`, channel.ID); err != nil {
		t.Fatal(err)
	}
	f.check(t, "GET", fmt.Sprintf("/v1/partner-links/%d/search?q=%s", link.ID, url.QueryEscape("工单")), reader.Token, nil, 200, &matches)
	f.check(t, "GET", root+"/mentions", reader.Token, nil, 200, &notifications)
	f.check(t, "GET", root+"/threads/2", reader.Token, nil, 404, nil)
	f.check(t, "GET", management+"/export-preview", host, nil, 200, &previewHost)
	if len(matches.Items) != 0 || len(notifications.Items) != 0 || previewHost.Items != 2 {
		t.Fatalf("expired root or descendant escaped filtering: search=%+v mentions=%+v export=%+v", matches, notifications, previewHost)
	}
	_, err = storage.ChangeSharedGrant(ctx, reader.Session.Scope, link.ID, "revoke", store.GrantChange{UserID: reader.Session.UserID, ExpectedVersion: partnerGrant.Version, Revoke: true})
	if err != nil {
		t.Fatal(err)
	}
	f.check(t, "GET", root+"/mentions", reader.Token, nil, 404, nil)
	f.check(t, "GET", "/v1/shared-projects", reader.Token, nil, 200, &projects)
	if len(projects.Items) != 0 {
		t.Fatal("revoked project still in directory")
	}
}
