package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/axin2024/messaging-cluster/internal/demoui"
)

func TestSharedWorkbenchOnMySQL(t *testing.T) {
	if os.Getenv("MESSAGING_BROWSER_TEST") != "1" {
		t.Skip("set MESSAGING_BROWSER_TEST=1 to run the real browser workbench acceptance")
	}
	dsn := testMySQLDSN(t)
	storage := syncTestStore(t, dsn)
	dir := t.TempDir()
	hostGateway := gatewayTestServerWithMedia(t, storage, "workbench-host", attachmentTestStorage(t, dir))
	peerGateway := gatewayTestServerWithMedia(t, storage, "workbench-peer", attachmentTestStorage(t, dir))
	hostHTTP := httptest.NewServer(demoui.Attach(hostGateway.Config.Handler))
	t.Cleanup(hostHTTP.Close)
	peerHTTP := httptest.NewServer(demoui.Attach(peerGateway.Config.Handler))
	t.Cleanup(peerHTTP.Close)
	buyerTenant, buyerAdmin := messageTestApp(t, storage)
	supplierTenant, supplierAdmin := messageTestApp(t, storage)
	f := identityFixture{handler: hostGateway.Config.Handler}
	buyer := messageTestUser(t, f, buyerTenant, buyerAdmin, "buyer")
	supplier := messageTestUser(t, f, supplierTenant, supplierAdmin, "supplier")
	input, err := json.Marshal(map[string]any{
		"host_origin": hostHTTP.URL, "peer_origin": peerHTTP.URL, "password": messageTestPassword,
		"host_tenant": buyerTenant, "host_admin": buyerAdmin, "host_user": buyer.Session.UserID,
		"peer_tenant": supplierTenant, "peer_admin": supplierAdmin, "peer_user": supplier.Session.UserID,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "node", "../../scripts/test-shared-workbench.mjs")
	command.Stdin = bytes.NewReader(input)
	output, err := command.Output()
	if err != nil {
		if failure, ok := err.(*exec.ExitError); ok {
			t.Fatalf("browser workbench: %v\n%s", err, failure.Stderr)
		}
		t.Fatal(err)
	}
	var browser struct {
		Link               uint64 `json:"link_id"`
		Channel            uint64 `json:"channel_id"`
		Seq                uint64 `json:"seq"`
		Epoch              uint64 `json:"grant_epoch"`
		RevokeResponse     int64  `json:"revoke_confirmed_ms"`
		AttachmentSent     int64  `json:"attachment_sent_ms"`
		SearchSent         int64  `json:"search_sent_ms"`
		ReconnectSent      int64  `json:"reconnect_sent_ms"`
		AuthorizationsSeen int    `json:"device_authorizations"`
	}
	if err = json.Unmarshal(output, &browser); err != nil || browser.Link == 0 || browser.Channel == 0 || browser.Seq == 0 || browser.AuthorizationsSeen != 3 {
		t.Fatalf("invalid browser evidence: %s, %v", output, err)
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var revokedMicros int64
	var epoch uint64
	var indexed, tickets, downloads int
	err = db.QueryRowContext(t.Context(), `SELECT TIMESTAMPDIFF(MICROSECOND,'1970-01-01 00:00:00',g.revoked_at),l.grant_epoch,
		(SELECT COUNT(*) FROM shared_search_index i WHERE i.channel_id=? AND i.seq=?),
		(SELECT COUNT(*) FROM shared_attachment_grants p WHERE p.channel_id=? AND p.seq=? AND p.expires_at>UTC_TIMESTAMP(6)),
		(SELECT COUNT(*) FROM shared_attachment_access a WHERE a.channel_id=? AND a.seq=?)
		FROM shared_grants g JOIN partner_links l ON l.id=g.link_id WHERE l.id=? AND g.user_id=?`,
		browser.Channel, browser.Seq, browser.Channel, browser.Seq, browser.Channel, browser.Seq, browser.Link, supplier.Session.UserID).
		Scan(&revokedMicros, &epoch, &indexed, &tickets, &downloads)
	if err != nil || epoch != browser.Epoch || indexed != 1 || tickets != 1 || downloads != 0 {
		t.Fatalf("browser A13 database evidence mismatch: epoch=%d index=%d live_tickets=%d authorized_downloads=%d error=%v", epoch, indexed, tickets, downloads, err)
	}
	revokedAt := time.UnixMicro(revokedMicros).UTC()
	if revokedAt.UnixMilli() > browser.RevokeResponse || browser.RevokeResponse > browser.AttachmentSent || browser.RevokeResponse > browser.SearchSent ||
		browser.RevokeResponse > browser.ReconnectSent {
		t.Fatalf("A13 requests did not start after committed revoke (%s): %s", revokedAt.Format(time.RFC3339Nano), output)
	}
	t.Logf("workbench A13 browser=%s mysql_revoked_at=%s indexed=%d live_tickets=%d download_receipts=%d",
		bytes.TrimSpace(output), revokedAt.Format(time.RFC3339Nano), indexed, tickets, downloads)
}
