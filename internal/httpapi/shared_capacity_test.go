package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/axin2024/messaging-cluster/internal/credential"
	"github.com/axin2024/messaging-cluster/internal/store"
)

const sharedCapacityMarker = "synthetic capacity procurement specification"

type capacityOrganization struct {
	key     string
	session store.Session
	token   string
	file    uint64
}

type capacitySpace struct {
	link, version  uint64
	path, cursor   string
	ticket         string
	organization   int
	partner        int
	verificationMS float64
}

func TestSharedCapacityOnMySQL(t *testing.T) {
	if os.Getenv("MESSAGING_SHARED_CAPACITY_TEST") != "full" {
		t.Skip("set MESSAGING_SHARED_CAPACITY_TEST=full for the 100-organization acceptance")
	}
	const organizations, spaces, workers = 100, 1000, 12
	dsn := testMySQLDSN(t)
	storage := syncTestStore(t, dsn)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var mysql string
	if err = db.QueryRow("SELECT VERSION()").Scan(&mysql); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	primary := gatewayTestServerWithMedia(t, storage, "shared-capacity-primary", attachmentTestStorage(t, dir))
	secondary := gatewayTestServerWithMedia(t, storage, "shared-capacity-secondary", attachmentTestStorage(t, dir))
	primaryClient, secondaryClient := primary.Client(), secondary.Client()
	primaryClient.Timeout, secondaryClient.Timeout = 5*time.Second, 5*time.Second
	ctx := t.Context()
	start := time.Now()
	hash, err := credential.HashPassword(messageTestPassword)
	if err != nil {
		t.Fatal(err)
	}
	orgs := make([]capacityOrganization, organizations)
	for i := range orgs {
		_, digest, err := credential.IssueToken()
		if err != nil {
			t.Fatal(err)
		}
		org := &orgs[i]
		org.key = "shared-cap-" + hex.EncodeToString(digest[:8])
		scope, err := storage.CreateApp(ctx, org.key, "dispatch", digest)
		if err != nil {
			t.Fatal(err)
		}
		account, err := storage.CreateUser(ctx, scope, "dispatcher", "Capacity operator", hash)
		if err != nil {
			t.Fatal(err)
		}
		token, deviceDigest, err := credential.IssueToken()
		if err != nil {
			t.Fatal(err)
		}
		org.token = token
		org.session, err = storage.LoginDevice(ctx, account, "desktop", deviceDigest)
		if err != nil {
			t.Fatal(err)
		}
		f := identityFixture{handler: primary.Config.Handler}
		file := createTestAttachment(t, f, org.token, "capacity.txt", sharedCapacityMarker)
		checkUpload(t, primary, fmt.Sprintf("/v1/attachments/%d/content", file.ID), org.token, sharedCapacityMarker, 200)
		org.file = file.ID
	}
	now := time.Now().UTC().Truncate(time.Second)
	ends := now.Add(2 * time.Hour)
	testSpaces := make([]capacitySpace, spaces)
	runSharedCapacity(t, "seed", spaces, workers, func(i int) error {
		hostN := i % len(orgs)
		peerN := (hostN + 1) % len(orgs)
		host, peer := orgs[hostN], orgs[peerN]
		key := fmt.Sprintf("space-%d", i)
		space, err := storage.CreateSpace(ctx, host.session.Scope, key, "Procurement capacity", "Synthetic collaboration acceptance")
		if err != nil {
			return fmt.Errorf("create space: %w", err)
		}
		offer := store.Offer{PeerTenant: peer.key, PeerApp: "dispatch", Scope: []string{"project:read", "thread:write"},
			SeatLimit: 5, AcceptBefore: now.Add(time.Hour), ProjectEndsAt: ends}
		invite, err := storage.InvitePartner(ctx, host.session.Scope, space.ID, "invite", offer)
		if err != nil {
			return fmt.Errorf("invite: %w", err)
		}
		link, err := storage.AcceptPartner(ctx, peer.session.Scope, space.ID, invite.ID, invite.PolicyRev, key, offer.Scope, false, offer.RetentionDays)
		if err != nil {
			return fmt.Errorf("accept: %w", err)
		}
		_, err = storage.ChangeSharedGrant(ctx, host.session.Scope, link.ID, "host", store.GrantChange{UserID: host.session.UserID, Role: "buyer", ExpiresAt: ends})
		if err != nil {
			return fmt.Errorf("grant host: %w", err)
		}
		member, err := storage.ChangeSharedGrant(ctx, peer.session.Scope, link.ID, "peer", store.GrantChange{UserID: peer.session.UserID, Role: "supplier", ExpiresAt: ends})
		if err != nil {
			return fmt.Errorf("grant partner: %w", err)
		}
		channel, err := storage.CreateSharedChannel(ctx, host.session.Scope, link.ID, store.SharedChannel{
			Key: "orders", Title: "Orders", Audience: "shared", HistoryFrom: "join",
		})
		if err != nil {
			return fmt.Errorf("create shared channel: %w", err)
		}
		seq, err := storage.SendSharedMessage(ctx, host.session, channel.ID, store.SharedMessageInput{ClientID: "order", Body: sharedCapacityMarker,
			Attachments: []uint64{host.file}})
		if err != nil {
			return fmt.Errorf("send: %w", err)
		}
		path := fmt.Sprintf("/v1/shared-channels/%d/messages", channel.ID)
		var page store.SharedPage
		if err = sharedCapacityRequest(ctx, primaryClient, primary.URL+path, "GET", peer.token, "", 200, &page); err != nil {
			return err
		}
		if len(page.Items) != 1 || page.Items[0].Body != sharedCapacityMarker || page.Next == "" {
			return fmt.Errorf("active device did not see its fixture")
		}
		var pass struct {
			Token string `json:"token"`
		}
		if err = sharedCapacityRequest(ctx, primaryClient, primary.URL+fmt.Sprintf("%s/%d/attachments/%d/grant", path, seq, host.file),
			"POST", peer.token, "", 201, &pass); err != nil {
			return err
		}
		testSpaces[i] = capacitySpace{link: link.ID, version: member.Version, path: path, cursor: page.Next,
			ticket: pass.Token, organization: hostN, partner: peerN}
		return nil
	})
	warmup := time.Since(start)
	primary.Server.Close()
	if err = primary.node.Close(); err != nil {
		t.Fatal(err)
	}
	recovered := gatewayTestServerWithMedia(t, storage, "shared-capacity-primary", attachmentTestStorage(t, dir))
	recoveredClient := recovered.Client()
	recoveredClient.Timeout = 5 * time.Second
	start = time.Now()
	runSharedCapacity(t, "revoke and reconnect", spaces, workers, func(i int) error {
		entry := &testSpaces[i]
		host, peer := orgs[entry.organization], orgs[entry.partner]
		sample := time.Now()
		_, err := storage.ChangeSharedGrant(ctx, host.session.Scope, entry.link, "revoke", store.GrantChange{
			UserID: peer.session.UserID, ExpectedVersion: entry.version, Revoke: true,
		})
		if err != nil {
			return err
		}
		if err = sharedCapacityRequest(ctx, recoveredClient, recovered.URL+entry.path+"?cursor="+entry.cursor, "GET", peer.token, "", 404, nil); err != nil {
			return err
		}
		search := fmt.Sprintf("/v1/partner-links/%d/search?q=procurement", entry.link)
		if err = sharedCapacityRequest(ctx, secondaryClient, secondary.URL+search, "GET", peer.token, "", 404, nil); err != nil {
			return err
		}
		if err = sharedCapacityRequest(ctx, secondaryClient, secondary.URL+"/v1/shared-attachment-content", "GET", peer.token, entry.ticket, 404, nil); err != nil {
			return err
		}
		entry.verificationMS = float64(time.Since(sample).Microseconds()) / 1000
		return nil
	})
	latencies := make([]float64, len(testSpaces))
	for i, fixture := range testSpaces {
		latencies[i] = fixture.verificationMS
	}
	var stillLive int
	if err = db.QueryRow(`SELECT COUNT(*) FROM shared_attachment_grants p JOIN shared_channels c ON c.id=p.channel_id
		JOIN partner_links l ON l.id=c.link_id JOIN shared_spaces s ON s.id=l.space_id JOIN tenants t ON t.id=s.host_tenant
		WHERE p.expires_at>UTC_TIMESTAMP(6) AND t.tenant_key LIKE 'shared-cap-%'`).Scan(&stillLive); err != nil || stillLive < spaces {
		t.Fatalf("attachment passes must still be within their TTL after the denial sample, count=%d error=%v", stillLive, err)
	}
	slices.Sort(latencies)
	t.Logf("shared capacity os=%s arch=%s cpu=%d go=%s mysql=%s organizations=%d spaces=%d workers=%d warmup=%s sample=%s authorized_warmup=%d denied_after_revoke=%d unexpected=0 timeouts=0 p95_revoke_and_verify_ms=%.3f",
		runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.Version(), mysql, organizations, spaces, workers, warmup.Round(time.Millisecond),
		time.Since(start).Round(time.Millisecond), spaces*2, spaces*3, latencies[(len(latencies)*95+99)/100-1])
}

func runSharedCapacity(t *testing.T, phase string, count, workers int, operation func(int) error) {
	t.Helper()
	queue, failures := make(chan int, count), make(chan error, count)
	for i := range count {
		queue <- i
	}
	close(queue)
	var group sync.WaitGroup
	for range workers {
		group.Go(func() {
			for item := range queue {
				if err := operation(item); err != nil {
					failures <- fmt.Errorf("space %d: %w", item, err)
				}
			}
		})
	}
	group.Wait()
	close(failures)
	if failed := len(failures); failed != 0 {
		var timedOut, logged int
		for err := range failures {
			if errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err) {
				timedOut++
			}
			if logged < 5 {
				t.Log(err)
				logged++
			}
		}
		t.Fatalf("%s failed=%d success=%d timeouts=%d", phase, failed, count-failed, timedOut)
	}
}

func sharedCapacityRequest(ctx context.Context, client *http.Client, url, method, token, ticket string, expected int, output any) error {
	request, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if ticket != "" {
		request.Header.Set("X-Attachment-Token", ticket)
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 32<<10))
	if err != nil {
		return err
	}
	if response.StatusCode != expected || (expected == 404 && !bytes.Equal(body, []byte("{\"error\":\"not_found\"}\n"))) {
		return fmt.Errorf("%s got %d, want %d with an empty denial payload", method, response.StatusCode, expected)
	}
	if output != nil {
		return json.Unmarshal(body, output)
	}
	return nil
}
