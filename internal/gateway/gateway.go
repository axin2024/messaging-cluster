package gateway

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/axin2024/messaging-cluster/internal/store"
)

const Protocol = "messaging.v1"

type Config struct {
	Lease          time.Duration
	Reconcile      time.Duration
	PingInterval   time.Duration
	PingTimeout    time.Duration
	MaxConnections int
}

func DefaultConfig() Config {
	return Config{Lease: 12 * time.Second, Reconcile: 500 * time.Millisecond,
		PingInterval: 15 * time.Second, PingTimeout: 5 * time.Second, MaxConnections: 4096}
}

type Gateway struct {
	store  *store.Store
	node   store.Node
	config Config
	logger *slog.Logger
	cancel context.CancelFunc
	done   chan struct{}
	ready  atomic.Bool
	err    error // read only after done closes

	mu       sync.Mutex
	stopping bool
	handlers int
	wait     sync.WaitGroup
	peers    map[uint64]*peer
}

type peer struct {
	connection store.Connection
	cancel     context.CancelCauseFunc
}

var (
	errSuperseded = errors.New("connection superseded")
	errInvalid    = errors.New("connection authority ended")
	errShutdown   = errors.New("gateway stopping")
	errHeartbeat  = errors.New("heartbeat timed out")
)

func Start(ctx context.Context, storage *store.Store, name string, config Config, logger *slog.Logger) (*Gateway, error) {
	if config.Reconcile <= 0 || config.Lease < 6*config.Reconcile || config.PingInterval <= 0 ||
		config.PingTimeout <= 0 || config.MaxConnections <= 0 {
		return nil, errors.New("invalid gateway timing or connection limit")
	}
	start, cancelStart := context.WithTimeout(ctx, 5*time.Second)
	defer cancelStart()
	node, err := storage.OpenNode(start, name, config.Lease)
	if err != nil {
		return nil, err
	}
	lifetime, cancel := context.WithCancel(ctx)
	g := &Gateway{store: storage, node: node, config: config, logger: logger, cancel: cancel,
		done: make(chan struct{}), peers: make(map[uint64]*peer)}
	g.ready.Store(true)
	go g.maintain(lifetime)
	return g, nil
}

func (g *Gateway) Done() <-chan struct{} { return g.done }
func (g *Gateway) Ready() bool           { return g.ready.Load() }

func (g *Gateway) Close() error {
	g.cancel()
	<-g.done
	return g.err
}

func (g *Gateway) maintain(ctx context.Context) {
	ticker := time.NewTicker(g.config.Reconcile)
	defer ticker.Stop()
	defer func() {
		g.ready.Store(false)
		g.mu.Lock()
		g.stopping = true
		for _, client := range g.peers {
			client.cancel(errShutdown)
		}
		g.mu.Unlock()
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		if err := g.store.StopNode(cleanup, g.node); err != nil {
			g.logger.Warn("could not record gateway stop", "node", g.node.ID, "error", err)
		}
		cancel()
		g.wait.Wait()
		close(g.done)
	}()

	lastRenewed := time.Now()
	nextPrune := lastRenewed.Add(time.Minute)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if time.Since(lastRenewed) >= g.config.Lease/3 {
			check, cancel := context.WithTimeout(ctx, 2*time.Second)
			err := g.store.RenewNode(check, g.node, g.config.Lease)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				g.unready(err)
				if errors.Is(err, store.ErrLeaseLost) || time.Since(lastRenewed) >= g.config.Lease {
					g.err = store.ErrLeaseLost
					return
				}
				continue
			}
			lastRenewed = time.Now()
		}
		active := g.snapshot()
		if len(active) > 0 {
			check, cancel := context.WithTimeout(ctx, 2*time.Second)
			live, err := g.store.LiveConnections(check, g.node)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				g.unready(err)
				continue
			}
			for _, client := range active {
				if live[client.connection.DeviceID] != client.connection.Epoch {
					client.cancel(errInvalid)
				}
			}
		}
		g.ready.Store(true)
		if time.Now().After(nextPrune) {
			prune, cancel := context.WithTimeout(ctx, 2*time.Second)
			if err := g.store.PruneGatewayTickets(prune); err != nil && ctx.Err() == nil {
				g.logger.Warn("could not prune gateway tickets", "node", g.node.ID, "error", err)
			}
			if err := g.store.PruneAttachmentGrants(prune); err != nil && ctx.Err() == nil {
				g.logger.Warn("could not prune attachment passes", "node", g.node.ID, "error", err)
			}
			cancel()
			nextPrune = time.Now().Add(time.Minute)
		}
	}
}

func (g *Gateway) unready(err error) {
	if g.ready.Swap(false) {
		g.logger.Warn("gateway could not verify active routes", "node", g.node.ID, "error", err)
	}
}

func (g *Gateway) snapshot() []*peer {
	g.mu.Lock()
	defer g.mu.Unlock()
	active := make([]*peer, 0, len(g.peers))
	for _, client := range g.peers {
		active = append(active, client)
	}
	return active
}

func (g *Gateway) reserve() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopping || !g.ready.Load() || g.handlers >= g.config.MaxConnections {
		return false
	}
	g.handlers++
	g.wait.Add(1)
	return true
}

func (g *Gateway) release() {
	g.mu.Lock()
	g.handlers--
	g.mu.Unlock()
	g.wait.Done()
}

func (g *Gateway) add(client *peer) error {
	id := client.connection.DeviceID
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopping {
		return errShutdown
	}
	if previous := g.peers[id]; previous != nil {
		if previous.connection.Epoch >= client.connection.Epoch {
			return errSuperseded
		}
		previous.cancel(errSuperseded)
	}
	g.peers[id] = client
	return nil
}

func (g *Gateway) remove(client *peer) {
	g.mu.Lock()
	defer g.mu.Unlock()
	id := client.connection.DeviceID
	if g.peers[id] == client {
		delete(g.peers, id)
	}
}
