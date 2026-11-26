package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/axin2024/messaging-cluster/internal/credential"
	"github.com/axin2024/messaging-cluster/internal/gateway"
	"github.com/axin2024/messaging-cluster/internal/media"
	"github.com/axin2024/messaging-cluster/internal/store"
)

type Server struct {
	store        *store.Store
	gateway      *gateway.Gateway
	logger       *slog.Logger
	passwordJobs chan struct{}
	uploadJobs   chan struct{}
	dummyHash    []byte
	blobs        media.Blobs
}

func New(storage *store.Store, cluster *gateway.Gateway, blobs media.Blobs, logger *slog.Logger) (http.Handler, error) {
	dummy, err := credential.HashPassword("unregistered-account-marker")
	if err != nil {
		return nil, err
	}
	s := &Server{store: storage, gateway: cluster, blobs: blobs, logger: logger,
		passwordJobs: make(chan struct{}, 4), uploadJobs: make(chan struct{}, 8), dummyHash: dummy}
	routes := http.NewServeMux()
	routes.HandleFunc("GET /healthz", s.health)
	routes.HandleFunc("GET /readyz", s.ready)
	routes.HandleFunc("POST /v1/admin/users", s.createUser)
	routes.HandleFunc("POST /v1/sessions", s.login)
	routes.HandleFunc("GET /v1/me", s.me)
	routes.HandleFunc("GET /v1/devices", s.listDevices)
	routes.HandleFunc("POST /v1/devices/current/rotate", s.rotate)
	routes.HandleFunc("DELETE /v1/devices/{id}", s.revoke)
	routes.HandleFunc("GET /v1/devices/{id}/presence", s.presence)
	routes.HandleFunc("GET /v1/devices/{id}/connections", s.connections)
	routes.HandleFunc("POST /v1/conversations/direct", s.openDirect)
	routes.HandleFunc("POST /v1/conversations/group", s.createGroup)
	routes.HandleFunc("GET /v1/conversations", s.listConversations)
	routes.HandleFunc("GET /v1/conversations/{id}/members", s.conversationRoute(s.listMembers))
	routes.HandleFunc("PUT /v1/conversations/{id}/members/{user}", s.conversationRoute(s.setMember))
	routes.HandleFunc("DELETE /v1/conversations/{id}/members/{user}", s.conversationRoute(s.removeMember))
	routes.HandleFunc("GET /v1/conversations/{id}/membership-operations", s.conversationRoute(s.membershipOperations))
	routes.HandleFunc("POST /v1/conversations/{id}/membership-batches", s.conversationRoute(s.prepareMembershipBatch))
	routes.HandleFunc("GET /v1/conversations/{id}/membership-batches/{client_id}", s.conversationRoute(s.membershipBatch))
	routes.HandleFunc("POST /v1/conversations/{id}/membership-batches/{client_id}/execute", s.conversationRoute(s.executeMembershipBatch))
	routes.HandleFunc("PUT /v1/conversations/{id}/owner", s.conversationRoute(s.transferGroup))
	routes.HandleFunc("POST /v1/conversations/{id}/messages", s.conversationRoute(s.sendMessage))
	routes.HandleFunc("GET /v1/conversations/{id}/messages", s.conversationRoute(s.listMessages))
	routes.HandleFunc("GET /v1/conversations/{id}/messages/{seq}", s.conversationRoute(s.getMessage))
	routes.HandleFunc("DELETE /v1/conversations/{id}/messages/{seq}", s.conversationRoute(s.retractMessage))
	routes.HandleFunc("GET /v1/conversations/{id}/messages/by-client/{client_id}", s.conversationRoute(s.getMessageByClientID))
	routes.HandleFunc("GET /v1/conversations/{id}/retractions", s.conversationRoute(s.retractions))
	routes.HandleFunc("POST /v1/conversations/{id}/messages/{seq}/attachments/{attachment}/grants", s.conversationRoute(s.attachmentGrant))
	routes.HandleFunc("POST /v1/attachments", s.createAttachment)
	routes.HandleFunc("PUT /v1/attachments/{attachment}/content", s.uploadAttachment)
	routes.HandleFunc("GET /v1/attachment-content", s.attachmentContent)
	routes.HandleFunc("GET /v1/inbox", s.inbox)
	routes.HandleFunc("GET /v1/conversations/{id}/sync", s.conversationRoute(s.sync))
	routes.HandleFunc("PUT /v1/conversations/{id}/sync", s.conversationRoute(s.acknowledgeSync))
	routes.HandleFunc("GET /v1/conversations/{id}/read", s.conversationRoute(s.readState))
	routes.HandleFunc("PUT /v1/conversations/{id}/read", s.conversationRoute(s.markRead))
	routes.HandleFunc("POST /v1/admin/conversations/{id}/retention", s.adminConversationRoute(s.expireHistory))
	routes.HandleFunc("GET /v1/admin/conversations/{id}/retention", s.adminConversationRoute(s.retentionRuns))
	routes.HandleFunc("GET /v1/admin/diagnostics", s.diagnostics)
	routes.HandleFunc("GET /v1/admin/conversations/{id}/messages/{seq}/delivery", s.adminConversationRoute(s.messageDelivery))
	if cluster != nil {
		routes.HandleFunc("POST /v1/gateway/tickets", s.gatewayTicket)
		routes.HandleFunc("GET /v1/gateway", s.upgradeGateway)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		routes.ServeHTTP(w, r)
	}), nil
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	write(w, http.StatusOK, map[string]bool{"alive": true})
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if s.gateway != nil && !s.gateway.Ready() {
		write(w, http.StatusServiceUnavailable, map[string]bool{"ready": false})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ready(ctx); err != nil {
		write(w, http.StatusServiceUnavailable, map[string]bool{"ready": false})
		return
	}
	write(w, http.StatusOK, map[string]bool{"ready": true})
}

func (s *Server) authSession(w http.ResponseWriter, r *http.Request) (store.Session, bool) {
	digest, valid := bearer(r)
	if !valid {
		s.fail(w, r, store.ErrUnauthorized)
		return store.Session{}, false
	}
	session, err := s.store.Session(r.Context(), digest)
	if err != nil {
		s.fail(w, r, err)
		return store.Session{}, false
	}
	return session, true
}

func (s *Server) authAdmin(w http.ResponseWriter, r *http.Request) (store.Scope, bool) {
	digest, valid := bearer(r)
	if !valid {
		s.fail(w, r, store.ErrUnauthorized)
		return store.Scope{}, false
	}
	scope, err := s.store.Admin(r.Context(), digest)
	if err != nil {
		s.fail(w, r, err)
		return store.Scope{}, false
	}
	return scope, true
}

func (s *Server) workPassword(w http.ResponseWriter) bool {
	select {
	case s.passwordJobs <- struct{}{}:
		return true
	default:
		w.Header().Set("Retry-After", "1")
		problem(w, http.StatusTooManyRequests, "password_workers_busy")
		return false
	}
}

func bearer(r *http.Request) ([32]byte, bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return [32]byte{}, false
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return [32]byte{}, false
	}
	return credential.TokenDigest(token)
}

func read(w http.ResponseWriter, r *http.Request, input any) bool {
	format, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || format != "application/json" {
		problem(w, http.StatusUnsupportedMediaType, "json_required")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	err = decoder.Decode(input)
	if err == nil {
		if trailing := decoder.Decode(new(json.RawMessage)); trailing != io.EOF {
			err = trailing
			if err == nil {
				err = errors.New("unexpected second JSON value")
			}
		}
	}
	if err == nil {
		return true
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		problem(w, http.StatusRequestEntityTooLarge, "payload_too_large")
	} else {
		problem(w, http.StatusBadRequest, "invalid_json")
	}
	return false
}

func write(w http.ResponseWriter, status int, response any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}

func problem(w http.ResponseWriter, status int, code string) {
	write(w, status, map[string]string{"error": code})
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrUnauthorized):
		w.Header().Set("WWW-Authenticate", "Bearer")
		problem(w, http.StatusUnauthorized, "unauthorized")
	case errors.Is(err, store.ErrNotFound):
		problem(w, http.StatusNotFound, "not_found")
	case errors.Is(err, store.ErrForbidden):
		problem(w, http.StatusForbidden, "forbidden")
	case errors.Is(err, store.ErrHistoryExpired):
		problem(w, http.StatusGone, "history_expired")
	case errors.Is(err, store.ErrInvalidAttachment):
		problem(w, http.StatusUnprocessableEntity, "attachment_unavailable")
	case errors.Is(err, store.ErrRateLimited):
		w.Header().Set("Retry-After", "1")
		problem(w, http.StatusTooManyRequests, "tenant_rate_limited")
	case errors.Is(err, store.ErrPayloadConflict):
		problem(w, http.StatusConflict, "client_id_payload_conflict")
	case errors.Is(err, store.ErrConflict):
		problem(w, http.StatusConflict, "already_exists")
	default:
		s.logger.Error("operation failed", "pattern", r.Pattern, "error", err)
		problem(w, http.StatusInternalServerError, "internal_error")
	}
}

func parseID(value string) (uint64, bool) {
	id, err := strconv.ParseUint(value, 10, 64)
	return id, err == nil && id != 0
}
