package httpapi

import (
	"net/http"

	"github.com/axin2024/messaging-cluster/internal/credential"
)

func (s *Server) sharedMentionCandidates(w http.ResponseWriter, r *http.Request) {
	session, ok := s.authSharedSession(w, r)
	if !ok {
		return
	}
	channel, valid := parseID(r.PathValue("channel"))
	var root uint64
	if value := r.URL.Query().Get("root_seq"); value != "" {
		var rootValid bool
		root, rootValid = parseID(value)
		valid = valid && rootValid
	}
	side := r.URL.Query().Get("side")
	role := r.URL.Query().Get("duty_role")
	if !valid || (side != "own" && side != "partner") || (role != "" && !credential.ValidKey(role)) {
		problem(w, 422, "invalid_mention_scope")
		return
	}
	after, ok := sharedPageAfter(w, r)
	if !ok {
		return
	}
	items, err := s.store.SharedMentionCandidates(r.Context(), session, channel, root, after, side, role)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, 200, map[string]any{"items": items})
}

func (s *Server) sharedMentions(w http.ResponseWriter, r *http.Request) {
	session, ok := s.authSharedSession(w, r)
	if !ok {
		return
	}
	channel, valid := parseID(r.PathValue("channel"))
	if !valid {
		problem(w, 422, "invalid_channel")
		return
	}
	after, ok := sharedPageAfter(w, r)
	if !ok {
		return
	}
	items, err := s.store.SharedMentions(r.Context(), session, channel, after)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, 200, map[string]any{"items": items})
}
