package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/axin2024/messaging-cluster/internal/credential"
	"github.com/axin2024/messaging-cluster/internal/media"
	"github.com/axin2024/messaging-cluster/internal/store"
)

func TestAttachmentAccessAndRetractionAcrossGatewaysOnMySQL(t *testing.T) {
	dsn := testMySQLDSN(t)
	storage := syncTestStore(t, dsn)
	root := t.TempDir()
	first := gatewayTestServerWithMedia(t, storage, "media-first", attachmentTestStorage(t, root))
	second := gatewayTestServerWithMedia(t, storage, "media-second", attachmentTestStorage(t, root))
	f1 := identityFixture{handler: first.Config.Handler}
	f2 := identityFixture{handler: second.Config.Handler}
	tenant, admin := messageTestApp(t, storage)
	owner := messageTestUser(t, f1, tenant, admin, "owner")
	member := messageTestUser(t, f1, tenant, admin, "member")
	late := messageTestUser(t, f1, tenant, admin, "late")
	otherTenant, otherAdmin := messageTestApp(t, storage)
	outsider := messageTestUser(t, f1, otherTenant, otherAdmin, "outsider")
	var group store.Conversation
	f1.check(t, "POST", "/v1/conversations/group", owner.Token, map[string]string{
		"title": "Shared documents", "history_policy": "since_join",
	}, http.StatusCreated, &group)
	base := fmt.Sprintf("/v1/conversations/%d", group.ID)
	messages := base + "/messages"
	memberPath := fmt.Sprintf("%s/members/%d", base, member.Session.UserID)
	role := map[string]string{"role": "member"}
	f1.check(t, "PUT", memberPath, owner.Token, role, http.StatusOK, nil)
	const original = "Immutable synthetic attachment"
	attachment := createTestAttachment(t, f1, owner.Token, "交接.txt", original)
	foreign := createTestAttachment(t, f1, outsider.Token, "external.txt", "External tenant")
	attachmentPath := fmt.Sprintf("/v1/attachments/%d/content", attachment.ID)
	foreignPath := fmt.Sprintf("/v1/attachments/%d/content", foreign.ID)
	mediaMessage := func(clientID string, id uint64) store.OutgoingMessage {
		return store.OutgoingMessage{ClientID: clientID, Body: "Attachment report", AttachmentIDs: []uint64{id}}
	}
	f1.check(t, "POST", messages, owner.Token, mediaMessage("pending-attachment", attachment.ID), http.StatusUnprocessableEntity, nil)
	checkUpload(t, first, attachmentPath, member.Token, original, http.StatusNotFound)
	checkUpload(t, first, attachmentPath, owner.Token, strings.Repeat("x", len(original)), http.StatusUnprocessableEntity)
	checkUpload(t, first, attachmentPath, owner.Token, "too short", http.StatusUnprocessableEntity)
	checkUpload(t, first, attachmentPath, owner.Token, original, http.StatusOK)
	checkUpload(t, second, attachmentPath, owner.Token, original, http.StatusOK)
	checkUpload(t, first, foreignPath, outsider.Token, "External tenant", http.StatusOK)
	f1.check(t, "POST", messages, owner.Token, mediaMessage("external-attachment", foreign.ID), http.StatusUnprocessableEntity, nil)
	f1.check(t, "POST", messages, member.Token, mediaMessage("wrong-owner", attachment.ID), http.StatusUnprocessableEntity, nil)

	input := mediaMessage("first-media", attachment.ID)
	var created store.MessageReceipt
	f1.check(t, "POST", messages, owner.Token, input, http.StatusCreated, &created)
	if created.Message.Seq != 1 || !slices.Equal(created.Message.AttachmentIDs, []uint64{attachment.ID}) {
		t.Fatalf("failed upload attempts consumed a sequence or attachment was missing: %+v", created)
	}
	grantPath := func(seq uint64) string {
		return fmt.Sprintf("%s/%d/attachments/%d/grants", messages, seq, attachment.ID)
	}
	initialGrant := issueTestGrant(t, f1, grantPath(1), member.Token)
	assertAttachmentDownload(t, second, initialGrant, "", http.StatusOK, original)
	assertAttachmentDownload(t, second, initialGrant, "bytes=0-8", http.StatusPartialContent, original[:9])
	f2.check(t, "POST", grantPath(1), outsider.Token, nil, http.StatusNotFound, nil)
	f2.check(t, "PUT", fmt.Sprintf("%s/members/%d", base, late.Session.UserID), owner.Token, role, http.StatusOK, nil)
	f2.check(t, "POST", grantPath(1), late.Token, nil, http.StatusNotFound, nil)
	f1.check(t, "POST", messages, owner.Token, mediaMessage("second-media", attachment.ID), http.StatusCreated, nil)
	lateGrant := issueTestGrant(t, f2, grantPath(2), late.Token)
	assertAttachmentDownload(t, first, lateGrant, "", http.StatusOK, original)
	f1.check(t, "DELETE", memberPath, member.Token, nil, http.StatusNoContent, nil)
	assertAttachmentDownload(t, second, initialGrant, "", http.StatusNotFound, "")
	f1.check(t, "PUT", memberPath, owner.Token, role, http.StatusOK, nil)
	assertAttachmentDownload(t, second, initialGrant, "", http.StatusNotFound, "")
	currentGrant := issueTestGrant(t, f1, grantPath(1), member.Token)
	assertAttachmentDownload(t, second, currentGrant, "", http.StatusOK, original)
	var rotated sessionResponse
	f2.check(t, "POST", "/v1/devices/current/rotate", member.Token, nil, http.StatusOK, &rotated)
	assertAttachmentDownload(t, first, currentGrant, "", http.StatusNotFound, "")
	withdrawalGrant := issueTestGrant(t, f2, grantPath(1), rotated.Token)
	assertAttachmentDownload(t, first, withdrawalGrant, "", http.StatusOK, original)

	f2.check(t, "DELETE", messages+"/1", rotated.Token, nil, http.StatusForbidden, nil)
	var retracted, retried store.Retraction
	f2.check(t, "DELETE", messages+"/1", owner.Token, nil, http.StatusOK, &retracted)
	f1.check(t, "DELETE", messages+"/1", owner.Token, nil, http.StatusOK, &retried)
	if retracted.ID == 0 || retracted.Seq != 1 || retracted.ActorID != owner.Session.UserID || retracted != retried {
		t.Fatalf("withdrawal did not preserve a stable audit event: %+v vs %+v", retracted, retried)
	}
	assertAttachmentDownload(t, first, withdrawalGrant, "", http.StatusNotFound, "")
	f1.check(t, "POST", grantPath(1), rotated.Token, nil, http.StatusNotFound, nil)
	assertAttachmentDownload(t, second, lateGrant, "", http.StatusOK, original)
	var pageAfter store.SyncPage
	f1.check(t, "GET", base+"/sync?after=0", rotated.Token, nil, http.StatusOK, &pageAfter)
	assertSyncPage(t, pageAfter, 2, false, 1, 2)
	assertRetractedPlaceholder(t, pageAfter.Items[0], 1)
	var receipt store.MessageReceipt
	f1.check(t, "POST", messages, owner.Token, input, http.StatusOK, &receipt)
	if !receipt.Duplicate {
		t.Fatalf("withdrawn send retry rewrote the original: %+v", receipt)
	}
	assertRetractedPlaceholder(t, receipt.Message, 1)
	var lookup store.Message
	f1.check(t, "GET", messages+"/by-client/first-media", owner.Token, nil, http.StatusOK, &lookup)
	assertRetractedPlaceholder(t, lookup, 1)
	var events page[store.Retraction]
	f2.check(t, "GET", base+"/retractions", rotated.Token, nil, http.StatusOK, &events)
	if len(events.Items) != 1 || events.Items[0].ID != retracted.ID || events.NextAfter != retracted.ID {
		t.Fatalf("offline device could not retrieve the retraction: %+v", events)
	}
	f2.check(t, "GET", base+"/retractions", late.Token, nil, http.StatusOK, &events)
	if len(events.Items) != 0 {
		t.Fatalf("withdrawal timeline leaked an invisible message: %+v", events)
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tokenDigest, _ := credential.TokenDigest(lateGrant)
	if _, err := db.ExecContext(t.Context(), `UPDATE attachment_grants SET expires_at = UTC_TIMESTAMP(6) - INTERVAL 1 SECOND
		WHERE token_digest = ?`, tokenDigest[:]); err != nil {
		t.Fatal(err)
	}
	assertAttachmentDownload(t, first, lateGrant, "", http.StatusNotFound, "")
	retentionGrant := issueTestGrant(t, f1, grantPath(2), late.Token)
	if _, err := db.ExecContext(t.Context(), `UPDATE messages SET created_at = UTC_TIMESTAMP(6) - INTERVAL 7 DAY
		WHERE conversation_id = ?`, group.ID); err != nil {
		t.Fatal(err)
	}
	f1.check(t, "POST", fmt.Sprintf("/v1/admin/conversations/%d/retention", group.ID), admin,
		map[string]time.Time{"before": time.Now().UTC().Add(-time.Hour)}, http.StatusOK, nil)
	assertAttachmentDownload(t, second, retentionGrant, "", http.StatusNotFound, "")
}

func attachmentTestStorage(t *testing.T, dir string) media.Blobs {
	t.Helper()
	storage, err := media.OpenLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	return storage
}

func createTestAttachment(t *testing.T, f identityFixture, token, name, body string) store.Attachment {
	t.Helper()
	digest := sha256.Sum256([]byte(body))
	var attachment store.Attachment
	f.check(t, "POST", "/v1/attachments", token, map[string]any{
		"filename": name, "content_type": "text/plain", "size": len(body), "sha256": hex.EncodeToString(digest[:]),
	}, http.StatusCreated, &attachment)
	if attachment.ID == 0 || attachment.ReadyAt != nil {
		t.Fatalf("invalid upload declaration: %+v", attachment)
	}
	return attachment
}

func issueTestGrant(t *testing.T, f identityFixture, path, token string) string {
	t.Helper()
	var grant struct {
		Token     string `json:"token"`
		ExpiresIn int64  `json:"expires_in"`
	}
	f.check(t, "POST", path, token, nil, http.StatusCreated, &grant)
	if _, valid := credential.TokenDigest(grant.Token); !valid || grant.ExpiresIn != 60 {
		t.Fatal("invalid download grant")
	}
	return grant.Token
}

func checkUpload(t *testing.T, gateway *gatewayFixture, path, token, body string, status int) {
	t.Helper()
	content, _ := attachmentHTTP(t, gateway, "PUT", path, "Bearer "+token, "", body, status)
	if status == http.StatusOK && !bytes.Contains(content, []byte(`"ready_at"`)) {
		t.Fatal("upload did not reach the ready state")
	}
}

func assertAttachmentDownload(t *testing.T, gateway *gatewayFixture, token, byteRange string, status int, expected string) {
	t.Helper()
	body, headers := attachmentHTTP(t, gateway, "GET", "/v1/attachment-content", "Attachment "+token, byteRange, "", status)
	if status != http.StatusOK && status != http.StatusPartialContent {
		return
	}
	if string(body) != expected || !strings.HasPrefix(headers.Get("Content-Disposition"), "attachment;") || headers.Get("Cache-Control") != "no-store" {
		t.Fatalf("download body or headers were incorrect: %q, %v", body, headers)
	}
}

func attachmentHTTP(t *testing.T, gateway *gatewayFixture, method, path, authorization, byteRange, body string, status int) ([]byte, http.Header) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, gateway.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", authorization)
	request.Header.Set("Range", byteRange)
	response, err := gateway.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	content, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != status {
		t.Fatalf("%s %s returned HTTP %d, expected %d: %q (%v)", method, path, response.StatusCode, status, content, err)
	}
	return content, response.Header
}

func assertRetractedPlaceholder(t *testing.T, message store.Message, seq uint64) {
	t.Helper()
	if message.Seq != seq || message.RetractedAt == nil || message.Body != "" || len(message.AttachmentIDs) != 0 {
		t.Fatalf("withdrawn message was not a redacted placeholder: %+v", message)
	}
}
