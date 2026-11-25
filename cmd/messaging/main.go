package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/axin2024/messaging-cluster/internal/credential"
	"github.com/axin2024/messaging-cluster/internal/demoui"
	"github.com/axin2024/messaging-cluster/internal/gateway"
	"github.com/axin2024/messaging-cluster/internal/httpapi"
	"github.com/axin2024/messaging-cluster/internal/media"
	"github.com/axin2024/messaging-cluster/internal/store"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(os.Args[1:], logger); err != nil {
		logger.Error("messaging command failed", "error", err)
		os.Exit(1)
	}
}

func run(args []string, logger *slog.Logger) error {
	if len(args) == 0 || (len(args) == 1 && (args[0] == "-h" || args[0] == "--help")) {
		fmt.Fprintln(os.Stdout, "Usage: messaging migrate | create-app TENANT APP | set-tenant-rate TENANT PER_SECOND | serve")
		return nil
	}
	dsn := os.Getenv("MESSAGING_MYSQL_DSN")
	switch args[0] {
	case "migrate":
		if len(args) != 1 {
			return errors.New("usage: messaging migrate")
		}
		version, err := store.Migrate(dsn)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]uint{"schema_version": version})
	case "create-app":
		if len(args) != 3 || !credential.ValidKey(args[1]) || !credential.ValidKey(args[2]) {
			return errors.New("usage: messaging create-app TENANT APP with lowercase app and tenant keys")
		}
		return createApp(dsn, args[1], args[2])
	case "set-tenant-rate":
		if len(args) != 3 || !credential.ValidKey(args[1]) {
			return errors.New("usage: messaging set-tenant-rate TENANT PER_SECOND")
		}
		rate, err := strconv.Atoi(args[2])
		if err != nil || rate < 1 || rate > 10000 {
			return errors.New("limit must be between 1 and 10000 messages per second")
		}
		return setTenantRate(dsn, args[1], rate)
	case "serve":
		if len(args) != 1 {
			return errors.New("usage: messaging serve")
		}
		return serve(dsn, logger)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func setTenantRate(dsn, tenant string, limit int) error {
	storage, err := store.Open(dsn)
	if err != nil {
		return err
	}
	defer storage.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := storage.Ready(ctx); err != nil {
		return err
	}
	if err := storage.SetTenantSendLimit(ctx, tenant, limit); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"tenant": tenant, "messages_per_second": limit})
}

func createApp(dsn, tenantKey, appKey string) error {
	storage, err := store.Open(dsn)
	if err != nil {
		return err
	}
	defer storage.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := storage.Ready(ctx); err != nil {
		return err
	}
	token, digest, err := credential.IssueToken()
	if err != nil {
		return err
	}
	scope, err := storage.CreateApp(ctx, tenantKey, appKey, digest)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
		store.Scope
		Tenant     string `json:"tenant"`
		App        string `json:"app"`
		AdminToken string `json:"admin_token"`
	}{Scope: scope, Tenant: tenantKey, App: appKey, AdminToken: token})
}

func serve(dsn string, logger *slog.Logger) error {
	storage, err := store.Open(dsn)
	if err != nil {
		return err
	}
	defer storage.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	readyCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err = storage.Ready(readyCtx)
	cancel()
	if err != nil {
		return err
	}
	nodeName := os.Getenv("MESSAGING_NODE_NAME")
	if nodeName == "" {
		nodeName = "gateway"
	}
	if !credential.ValidKey(nodeName) {
		return errors.New("invalid MESSAGING_NODE_NAME")
	}
	blobPath := os.Getenv("MESSAGING_ATTACHMENT_DIR")
	if blobPath == "" {
		blobPath = "storage/attachments"
	}
	blobs, err := media.OpenLocal(blobPath)
	if err != nil {
		return fmt.Errorf("open attachment store: %w", err)
	}
	defer blobs.Close()
	cluster, err := gateway.Start(ctx, storage, nodeName, gateway.DefaultConfig(), logger)
	if err != nil {
		return err
	}
	defer cluster.Close()
	handler, err := httpapi.New(storage, cluster, blobs, logger)
	if err != nil {
		return err
	}
	if os.Getenv("MESSAGING_DEMO_UI") == "1" {
		handler = demoui.Attach(handler)
	}
	addr := os.Getenv("MESSAGING_HTTP_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	server := &http.Server{
		Addr: addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 20 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 64 << 10,
	}
	done := make(chan error, 1)
	go func() {
		logger.Info("HTTP server starting", "address", addr)
		done <- server.ListenAndServe()
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	case <-cluster.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return errors.Join(server.Shutdown(shutdownCtx), cluster.Close())
}
