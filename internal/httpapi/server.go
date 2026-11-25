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
	"github.com/axin2024/messaging-cluster/internal/store"
)

type Server struct {
	store        *store.Store
	logger       *slog.Logger
	passwordJobs chan struct{}
	dummyHash    []byte
}

func New(storage *store.Store, logger *slog.Logger) (http.Handler, error) {
	dummy, err := credential.HashPassword("unregistered-account-marker")
	if err != nil {
		return nil, err
	}
	s := &Server{store: storage, logger: logger, passwordJobs: make(chan struct{}, 4), dummyHash: dummy}
	routes := http.NewServeMux()
	routes.HandleFunc("GET /healthz", s.health)
	routes.HandleFunc("GET /readyz", s.ready)
	routes.HandleFunc("POST /v1/admin/users", s.createUser)
	routes.HandleFunc("POST /v1/sessions", s.login)
	routes.HandleFunc("GET /v1/me", s.me)
	routes.HandleFunc("GET /v1/devices", s.listDevices)
	routes.HandleFunc("POST /v1/devices/current/rotate", s.rotate)
	routes.HandleFunc("DELETE /v1/devices/{id}", s.revoke)
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
