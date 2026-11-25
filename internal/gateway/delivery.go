package gateway

import (
	"context"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/axin2024/messaging-cluster/internal/store"
)

const DeliveryWindow = 16

type deliveryWindow struct {
	mu       sync.Mutex
	awaiting map[uint64]bool // false while queued, true once the socket writer owns the frame
	degraded bool
	queue    chan store.Delivery
	resync   chan struct{}
}

func newDeliveryWindow() *deliveryWindow {
	return &deliveryWindow{awaiting: make(map[uint64]bool), queue: make(chan store.Delivery, DeliveryWindow), resync: make(chan struct{}, 1)}
}

func (w *deliveryWindow) offer(event store.Delivery) []uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.degraded && len(w.awaiting) < DeliveryWindow {
		select {
		case w.queue <- event:
			w.awaiting[event.ID] = false
			return nil
		default:
		}
	}
	dropped := []uint64{event.ID}
	if !w.degraded {
		w.degraded = true
		for id := range w.awaiting {
			dropped = append(dropped, id)
		}
		clear(w.awaiting)
		w.resync <- struct{}{}
	}
	return dropped
}

func (w *deliveryWindow) beginWrite(id uint64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, found := w.awaiting[id]; !found {
		return false
	}
	w.awaiting[id] = true
	return true
}

func (w *deliveryWindow) canAck(id uint64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.awaiting[id]
}

func (w *deliveryWindow) ack(id uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.awaiting, id)
}

func (g *Gateway) deliver(ctx context.Context) {
	defer close(g.deliveryDone)
	poll := time.NewTicker(g.config.DeliveryInterval)
	expire := time.NewTicker(5 * time.Second)
	defer poll.Stop()
	defer expire.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-expire.C:
			g.deliveryAction(ctx, "expire", g.store.ExpireDeliveries)
		case <-poll.C:
			if g.Ready() {
				g.deliveryAction(ctx, "route", g.store.MaterializeDelivery)
				g.deliveryAction(ctx, "offer", g.offerDeliveries)
			}
		}
	}
}

func (g *Gateway) deliveryAction(ctx context.Context, stage string, action func(context.Context) error) {
	operation, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := action(operation); err != nil && ctx.Err() == nil {
		g.logger.Warn("notification worker failed", "stage", stage, "node", g.node.ID, "error", err)
	}
}

func (g *Gateway) offerDeliveries(ctx context.Context) error {
	deliveries, err := g.store.OfferDeliveries(ctx, g.node)
	if err != nil {
		return err
	}
	var dropped []uint64
	for _, event := range deliveries {
		g.mu.Lock()
		client := g.peers[event.DeviceID]
		g.mu.Unlock()
		if client == nil || client.connection.Epoch != event.Epoch {
			dropped = append(dropped, event.ID)
			continue
		}
		dropped = append(dropped, client.delivery.offer(event)...)
	}
	return g.store.DegradeDeliveries(ctx, dropped)
}

func (g *Gateway) notify(ctx context.Context, socket *websocket.Conn, client *peer) {
	for {
		var frame any
		var id uint64
		select {
		case <-ctx.Done():
			return
		case <-client.delivery.resync:
			frame = map[string]string{"type": "notification.resync_required"}
		case event := <-client.delivery.queue:
			if !client.delivery.beginWrite(event.ID) {
				continue
			}
			id = event.ID
			frame = struct {
				Type string `json:"type"`
				store.Delivery
			}{Type: "notification." + event.Kind, Delivery: event}
		}
		write, cancel := context.WithTimeout(ctx, time.Second)
		err := wsjson.Write(write, socket, frame)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				client.cancel(errSlow)
			}
			return
		}
		if id != 0 {
			g.deliveryAction(ctx, "sent", func(operation context.Context) error { return g.store.MarkDeliverySent(operation, id) })
		}
	}
}
