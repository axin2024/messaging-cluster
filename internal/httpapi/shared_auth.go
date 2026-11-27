package httpapi

import (
	"net/http"

	"github.com/axin2024/messaging-cluster/internal/store"
)

func (s *Server) sharedEnabled(w http.ResponseWriter, r *http.Request, tenant uint64) bool {
	enabled, err := s.store.TenantSharedEnabled(r.Context(), tenant)
	if err != nil {
		s.fail(w, r, err)
		return false
	}
	if !enabled {
		s.fail(w, r, store.ErrNotFound)
	}
	return enabled
}

func (s *Server) authSharedAdmin(w http.ResponseWriter, r *http.Request) (store.Scope, bool) {
	scope, ok := s.authAdmin(w, r)
	return scope, ok && s.sharedEnabled(w, r, scope.TenantID)
}

func (s *Server) authSharedSession(w http.ResponseWriter, r *http.Request) (store.Session, bool) {
	session, ok := s.authSession(w, r)
	return session, ok && s.sharedEnabled(w, r, session.TenantID)
}
