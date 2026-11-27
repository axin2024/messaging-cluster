package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
)

func (s *Server) searchSharedMessages(w http.ResponseWriter, r *http.Request) {
	session, ok := s.authSharedSession(w, r)
	if !ok {
		return
	}
	link, valid := parseID(r.PathValue("link"))
	params := r.URL.Query()
	query := strings.TrimSpace(params.Get("q"))
	limit := 30
	if value := params.Get("limit"); value != "" {
		var err error
		limit, err = strconv.Atoi(value)
		valid = valid && err == nil && limit >= 1 && limit <= 100
	}
	if !valid || !utf8.ValidString(query) || query == "" || utf8.RuneCountInString(query) > 128 {
		problem(w, 422, "invalid_shared_search")
		return
	}
	page, err := s.store.SearchSharedMessages(r.Context(), session, link, query, params.Get("cursor"), limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, 200, page)
}

func (s *Server) hideSharedMessage(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.authSharedAdmin(w, r)
	if !ok {
		return
	}
	link, linkValid := parseID(r.PathValue("link"))
	channel, channelValid := parseID(r.PathValue("channel"))
	seq, seqValid := parseID(r.PathValue("seq"))
	if !linkValid || !channelValid || !seqValid {
		problem(w, 422, "invalid_message_reference")
		return
	}
	if err := s.store.HideSharedMessage(r.Context(), actor, link, channel, seq); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
