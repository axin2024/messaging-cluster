package httpapi

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/axin2024/messaging-cluster/internal/credential"
	"github.com/axin2024/messaging-cluster/internal/gateway"
	"github.com/axin2024/messaging-cluster/internal/store"
)

const capacityRate = 100
const capacityBody = "Synthetic dispatch for gateway capacity verification."

type capacityChat struct {
	path             string
	sender, receiver string
}

func TestCapacityOnMySQL(t *testing.T) {
	mode := os.Getenv("MESSAGING_CAPACITY_TEST")
	connections, warmSeconds, steadySeconds := 1000, 10, 600
	switch mode {
	case "":
		t.Skip("set MESSAGING_CAPACITY_TEST=full for the 10-minute acceptance run")
	case "smoke":
		connections, warmSeconds, steadySeconds = 40, 2, 10
	case "short":
		steadySeconds = 60
	case "full":
	default:
		t.Fatalf("unknown capacity mode %q", mode)
	}
	storage := syncTestStore(t, testMySQLDSN(t))
	first := capacityGateway(t, storage, "capacity-a")
	second := capacityGateway(t, storage, "capacity-b")
	client := second.Client()
	client.Timeout = 5 * time.Second
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	admin, digest, err := credential.IssueToken()
	if err != nil {
		t.Fatal(err)
	}
	scope, err := storage.CreateApp(ctx, "capacity-"+hex.EncodeToString(digest[:6]), "dispatch", digest)
	if err != nil {
		t.Fatal(err)
	}
	fixtureHash, err := credential.HashPassword(messageTestPassword)
	if err != nil {
		t.Fatal(err)
	}
	createSession := func(name string) (store.Session, string) {
		account, err := storage.CreateUser(ctx, scope, name, name, fixtureHash)
		if err != nil {
			t.Fatal(err)
		}
		token, digest, err := credential.IssueToken()
		if err != nil {
			t.Fatal(err)
		}
		session, err := storage.LoginDevice(ctx, account, "desktop", digest)
		if err != nil {
			t.Fatal(err)
		}
		return session, token
	}
	var open sync.WaitGroup
	defer func() { cancel(); open.Wait() }()
	var hints, degraded, unexpected atomic.Uint64
	var stopping atomic.Bool
	socketErrors := make(chan error, 1)
	track := func(socket *websocket.Conn, conversation uint64, onFirst bool) {
		open.Go(func() {
			for {
				var event deliveryFrame
				err := wsjson.Read(ctx, socket, &event)
				if err == nil && event.Type == "notification.resync_required" {
					degraded.Add(1)
					continue
				}
				if err == nil && (event.Type != "notification.message" || event.ID == 0 || event.ConversationID != conversation) {
					err = fmt.Errorf("unexpected event for conversation %d: %+v", conversation, event)
				}
				if err == nil {
					err = wsjson.Write(ctx, socket, map[string]any{"type": "notification.ack", "delivery_id": event.ID})
				}
				if err != nil {
					if ctx.Err() == nil && !(onFirst && stopping.Load()) {
						unexpected.Add(1)
						select {
						case socketErrors <- err:
						default:
						}
					}
					return
				}
				hints.Add(1)
			}
		})
	}
	chats := make([]capacityChat, connections/2)
	type failedPeer struct {
		token string
		chat  uint64
		epoch uint64
	}
	var failoverPeers []failedPeer
	for i := range chats {
		sender, senderToken := createSession(fmt.Sprintf("sender-%d", i))
		receiver, receiverToken := createSession(fmt.Sprintf("receiver-%d", i))
		chat, err := storage.OpenDirect(ctx, sender, receiver.UserID)
		if err != nil {
			t.Fatal(err)
		}
		chats[i] = capacityChat{fmt.Sprintf("/v1/conversations/%d", chat.ID), senderToken, receiverToken}
		firstToken, secondToken := receiverToken, senderToken
		if i%2 == 1 {
			firstToken, secondToken = senderToken, receiverToken
		}
		socketA, old := dialGateway(t, first.URL, firstToken, "")
		socketB, _ := dialGateway(t, second.URL, secondToken, "")
		track(socketA, chat.ID, true)
		track(socketB, chat.ID, false)
		failoverPeers = append(failoverPeers, failedPeer{firstToken, chat.ID, old.Epoch})
	}
	identify := func() store.Diagnostics {
		var result store.Diagnostics
		if err := capacityRequest(ctx, client, "GET", second.URL+"/v1/admin/diagnostics", admin, "", http.StatusOK, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	active := identify()
	if len(active.Nodes) != 2 || active.Nodes[0].Active != uint64(connections/2) || active.Nodes[1].Active != uint64(connections/2) {
		t.Fatalf("did not establish the intended connections: %+v", active.Nodes)
	}
	t.Logf("mode=%s os=%s arch=%s logical_cpus=%d gateways=2 websocket_connections=%d short_message_bytes=%d writers=16 rate=%d/s warmup=%ds stable=%ds",
		mode, runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), connections, len(capacityBody), capacityRate, warmSeconds, steadySeconds)
	origins := [2]string{first.URL, second.URL}
	warmCount, steadyCount := warmSeconds*capacityRate, steadySeconds*capacityRate
	runCapacityTraffic(t, client, origins, chats, "warm", 0, warmCount)
	runCapacityTraffic(t, client, origins, chats, "load", warmCount, steadyCount)
	total := warmCount + steadyCount
	var finished store.Diagnostics
	deadline := time.Now().Add(15 * time.Second)
	for {
		finished = identify()
		if finished.Delivery.Acked >= uint64(total) || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if finished.CommittedMessages != uint64(total) || finished.PendingOutbox != 0 || unexpected.Load() != 0 || len(finished.Nodes) != 2 ||
		finished.Nodes[0].Active != uint64(connections/2) || finished.Nodes[1].Active != uint64(connections/2) {
		select {
		case err := <-socketErrors:
			t.Logf("first unexpected socket error: %v", err)
		default:
		}
		t.Fatalf("unstable window: unexpected_socket_closes=%d diagnostics=%+v", unexpected.Load(), finished)
	}
	if hints.Load() != uint64(total) || finished.Delivery.Acked != uint64(total) || degraded.Load() != 0 {
		t.Errorf("notification delivery: total=%d received=%d acked=%d degraded_connections=%d", total, hints.Load(), finished.Delivery.Acked, degraded.Load())
	}
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	t.Logf("notification_receipts=%d notification_acks=%d degraded_connections=%d unexpected_socket_closes=%d go_heap_alloc_mib=%.1f go_sys_mib=%.1f after_stable_window",
		hints.Load(), finished.Delivery.Acked, degraded.Load(), unexpected.Load(), float64(memory.HeapAlloc)/(1<<20), float64(memory.Sys)/(1<<20))
	for i, chat := range chats {
		verifyCapacityHistory(t, client, second.URL, chat, i, len(chats), warmCount, total)
	}
	t.Logf("persisted_history_verified=%d missing_committed_messages=0", total)

	// Hold a real device checkpoint, stop A, and write while its receiver is offline.
	chat := chats[0]
	high := uint64((total + len(chats) - 1) / len(chats))
	if err := capacityRequest(ctx, client, "PUT", second.URL+chat.path+"/sync", chat.receiver, fmt.Sprintf(`{"seq":%d}`, high), http.StatusOK, nil); err != nil {
		t.Fatal(err)
	}
	stopping.Store(true)
	cutover := time.Now()
	if err := first.node.Close(); err != nil {
		t.Fatal(err)
	}
	var receipt store.MessageReceipt
	if err := capacityRequest(ctx, client, "POST", second.URL+chat.path+"/messages", chat.sender, `{"client_id":"during-cutover","body":"Offline dispatch"}`, http.StatusCreated, &receipt); err != nil {
		t.Fatal(err)
	}
	firstRecovered := time.Duration(0)
	for i, peer := range failoverPeers {
		socket, current := dialGateway(t, second.URL, peer.token, "")
		if current.Epoch != peer.epoch+1 || current.Node.ID != active.Nodes[1].ID {
			t.Fatalf("failed to advance epoch for peer %d: %+v", i, current)
		}
		track(socket, peer.chat, false)
		if i == 0 {
			var page store.SyncPage
			if err := capacityRequest(ctx, client, "GET", second.URL+chat.path+"/sync", chat.receiver, "", http.StatusOK, &page); err != nil {
				t.Fatal(err)
			}
			if len(page.Items) != 1 || page.Items[0].Seq != high+1 || receipt.Message.Seq != high+1 || page.Items[0].ClientID != "during-cutover" || page.HasMore {
				t.Fatalf("cutover sync did not return exactly the offline message: %+v", page)
			}
			firstRecovered = time.Since(cutover)
		}
	}
	allRecovered := time.Since(cutover)
	final := identify()
	if len(final.Nodes) != 2 || final.Nodes[0].Healthy || final.Nodes[0].Active != 0 || final.Nodes[1].Active != uint64(connections) || final.Nodes[1].Reconnects != uint64(connections/2) {
		t.Fatalf("unexpected recovery diagnostics: %+v", final.Nodes)
	}
	t.Logf("gateway_a_stopped=true reconnected=%d first_receiver_reconnected_and_synced=%s serial_reconnect_all=%s",
		len(failoverPeers), firstRecovered.Round(time.Millisecond), allRecovered.Round(time.Millisecond))
}

func capacityGateway(t *testing.T, storage *store.Store, name string) *gatewayFixture {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	node, err := gateway.Start(t.Context(), storage, name, gateway.DefaultConfig(), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := node.Close(); err != nil {
			t.Error(err)
		}
	})
	handler, err := New(storage, node, nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &gatewayFixture{Server: server, node: node}
}

func runCapacityTraffic(t *testing.T, client *http.Client, origins [2]string, chats []capacityChat, label string, base, count int) {
	t.Helper()
	const workers = 16
	ctx := t.Context()
	jobs := make(chan int, workers*2)
	latencies := make([]time.Duration, count)
	var failures atomic.Uint64
	var group sync.WaitGroup
	for range workers {
		group.Go(func() {
			for i := range jobs {
				started := time.Now()
				chat := chats[(base+i)%len(chats)]
				id := fmt.Sprintf("%s-%d", label, base+i)
				payload := fmt.Sprintf(`{"client_id":%q,"body":%q}`, id, capacityBody)
				var receipt store.MessageReceipt
				err := capacityRequest(ctx, client, "POST", origins[i%2]+chat.path+"/messages", chat.sender, payload, http.StatusCreated, &receipt)
				latencies[i] = time.Since(started)
				if err == nil && (receipt.Message.ClientID != id || receipt.Message.Seq != uint64((base+i)/len(chats)+1) || receipt.Duplicate) {
					err = fmt.Errorf("unexpected durable receipt for %s: %+v", id, receipt)
				}
				if err != nil && failures.Add(1) <= 3 {
					t.Logf("%s request %d: %v", label, base+i, err)
				}
			}
		})
	}
	started := time.Now()
	var maxIssueLag time.Duration
	for i := range count {
		when := started.Add(time.Duration(i) * time.Second / capacityRate)
		if until := time.Until(when); until > 0 {
			time.Sleep(until)
		}
		jobs <- i
		maxIssueLag = max(maxIssueLag, time.Since(when))
	}
	close(jobs)
	group.Wait()
	if remaining := time.Until(started.Add(time.Duration(count) * time.Second / capacityRate)); remaining > 0 {
		time.Sleep(remaining)
	}
	wall := time.Since(started)
	slices.Sort(latencies)
	t.Logf("phase=%s submitted=%d confirmed=%d wall=%s rate=%.2f/s max_issue_lag=%s receipt_p50=%s receipt_p95=%s receipt_p99=%s request_errors=%d",
		label, count, uint64(count)-failures.Load(), wall.Round(time.Millisecond), float64(count)/wall.Seconds(), maxIssueLag.Round(time.Millisecond),
		latencies[(count-1)*50/100].Round(time.Microsecond), latencies[(count-1)*95/100].Round(time.Microsecond),
		latencies[(count-1)*99/100].Round(time.Microsecond), failures.Load())
	if failures.Load() != 0 || maxIssueLag > time.Second {
		t.Fatalf("traffic phase %s missed receipt or pacing requirement", label)
	}
}

func verifyCapacityHistory(t *testing.T, client *http.Client, origin string, chat capacityChat, index, chats, warm, total int) {
	t.Helper()
	var cursor uint64
	for {
		var history store.SyncPage
		url := fmt.Sprintf("%s%s/sync?after=%d", origin, chat.path, cursor)
		if err := capacityRequest(t.Context(), client, "GET", url, chat.receiver, "", http.StatusOK, &history); err != nil {
			t.Fatal(err)
		}
		for _, item := range history.Items {
			i := int(cursor)*chats + index
			label := "load"
			if i < warm {
				label = "warm"
			}
			cursor++
			if i >= total || item.Seq != cursor || item.ClientID != fmt.Sprintf("%s-%d", label, i) || item.Body != capacityBody {
				t.Fatalf("history mismatch at expected %s-%d (seq %d): %+v", label, i, cursor, item)
			}
		}
		if len(history.Expired) != 0 || history.NextAfter != cursor || (history.HasMore && len(history.Items) == 0) {
			t.Fatalf("invalid sync page for chat %d: %+v", index, history)
		}
		if !history.HasMore {
			if int(cursor)*chats+index < total {
				t.Fatalf("chat %d stopped at sequence %d with expected remaining messages", index, cursor)
			}
			return
		}
	}
}

func capacityRequest(ctx context.Context, client *http.Client, method, url, token, body string, want int, output any) error {
	request, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != want {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		return fmt.Errorf("%s %s returned %d instead of %d: %s", method, request.URL.Path, response.StatusCode, want, body)
	}
	if output == nil {
		_, err := io.Copy(io.Discard, response.Body)
		return err
	}
	return json.NewDecoder(response.Body).Decode(output)
}
