package httpapi

import (
	"net/http"
	"strconv"
)

func sharedPageAfter(w http.ResponseWriter, r *http.Request) (uint64, bool) {
	value := r.URL.Query().Get("after")
	if value == "" {
		return 0, true
	}
	id, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		problem(w, 422, "invalid_after")
		return 0, false
	}
	return id, true
}

func (s *Server) adminSharedSpaces(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.authSharedAdmin(w, r)
	if !ok {
		return
	}
	after, ok := sharedPageAfter(w, r)
	if !ok {
		return
	}
	items, err := s.store.AdminSharedSpaces(r.Context(), actor, after)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, 200, map[string]any{"items": items})
}

func (s *Server) adminProjectLinks(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.authSharedAdmin(w, r)
	if !ok {
		return
	}
	after, ok := sharedPageAfter(w, r)
	if !ok {
		return
	}
	items, err := s.store.AdminProjectLinks(r.Context(), actor, after)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, 200, map[string]any{"items": items})
}

func (s *Server) memberProjectLinks(w http.ResponseWriter, r *http.Request) {
	session, ok := s.authSharedSession(w, r)
	if !ok {
		return
	}
	after, ok := sharedPageAfter(w, r)
	if !ok {
		return
	}
	items, err := s.store.MemberProjectLinks(r.Context(), session, after)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, 200, map[string]any{"items": items})
}

func (s *Server) adminSharedGrants(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.authSharedAdmin(w, r)
	if !ok {
		return
	}
	link, valid := parseID(r.PathValue("link"))
	if !valid {
		problem(w, 422, "invalid_link")
		return
	}
	after, ok := sharedPageAfter(w, r)
	if !ok {
		return
	}
	items, err := s.store.AdminSharedGrants(r.Context(), actor, link, after)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, 200, map[string]any{"items": items})
}
