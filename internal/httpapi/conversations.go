package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/axin2024/messaging-cluster/internal/store"
)

func (s *Server) conversationRoute(handler func(http.ResponseWriter, *http.Request, store.Session, uint64)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, ok := s.authSession(w, r)
		if !ok {
			return
		}
		id, ok := parseID(r.PathValue("id"))
		if !ok {
			problem(w, http.StatusBadRequest, "invalid_conversation")
			return
		}
		handler(w, r, session, id)
	}
}

func (s *Server) openDirect(w http.ResponseWriter, r *http.Request) {
	session, ok := s.authSession(w, r)
	if !ok {
		return
	}
	var input struct {
		UserID uint64 `json:"user_id"`
	}
	if !read(w, r, &input) {
		return
	}
	if input.UserID == 0 || input.UserID == session.UserID {
		problem(w, http.StatusBadRequest, "invalid_peer")
		return
	}
	conversation, err := s.store.OpenDirect(r.Context(), session, input.UserID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, http.StatusOK, conversation)
}

func (s *Server) createGroup(w http.ResponseWriter, r *http.Request) {
	session, ok := s.authSession(w, r)
	if !ok {
		return
	}
	var input struct {
		Title         string `json:"title"`
		HistoryPolicy string `json:"history_policy"`
	}
	if !read(w, r, &input) {
		return
	}
	input.Title = strings.TrimSpace(input.Title)
	if input.Title == "" || utf8.RuneCountInString(input.Title) > 100 ||
		(input.HistoryPolicy != "all" && input.HistoryPolicy != "since_join") {
		problem(w, http.StatusBadRequest, "invalid_group")
		return
	}
	conversation, err := s.store.CreateGroup(r.Context(), session, input.Title, input.HistoryPolicy)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, http.StatusCreated, conversation)
}

func afterCursor(w http.ResponseWriter, r *http.Request) (uint64, bool) {
	if cursor := r.URL.Query().Get("after"); cursor != "" {
		value, err := strconv.ParseUint(cursor, 10, 64)
		if err != nil {
			problem(w, http.StatusBadRequest, "invalid_cursor")
			return 0, false
		}
		return value, true
	}
	return 0, true
}

func (s *Server) listConversations(w http.ResponseWriter, r *http.Request) {
	session, ok := s.authSession(w, r)
	if !ok {
		return
	}
	after, ok := afterCursor(w, r)
	if !ok {
		return
	}
	items, err := s.store.Conversations(r.Context(), session, after)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(items) == 50 {
		after = items[len(items)-1].ID
	} else {
		after = 0
	}
	write(w, http.StatusOK, page[store.Conversation]{Items: items, NextAfter: after})
}

type page[T any] struct {
	Items     []T    `json:"items"`
	NextAfter uint64 `json:"next_after,omitempty"`
}

func (s *Server) listMembers(w http.ResponseWriter, r *http.Request, session store.Session, id uint64) {
	after, ok := afterCursor(w, r)
	if !ok {
		return
	}
	items, err := s.store.Members(r.Context(), session, id, after)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(items) == 50 {
		after = items[len(items)-1].UserID
	} else {
		after = 0
	}
	write(w, http.StatusOK, page[store.Membership]{Items: items, NextAfter: after})
}

func (s *Server) setMember(w http.ResponseWriter, r *http.Request, session store.Session, id uint64) {
	user, ok := parseID(r.PathValue("user"))
	if !ok {
		problem(w, http.StatusBadRequest, "invalid_user")
		return
	}
	var input struct {
		Role string `json:"role"`
	}
	if !read(w, r, &input) {
		return
	}
	if input.Role != "member" && input.Role != "admin" {
		problem(w, http.StatusBadRequest, "invalid_role")
		return
	}
	member, err := s.store.SetMember(r.Context(), session, id, user, input.Role)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, http.StatusOK, member)
}

func (s *Server) removeMember(w http.ResponseWriter, r *http.Request, session store.Session, id uint64) {
	user, ok := parseID(r.PathValue("user"))
	if !ok {
		problem(w, http.StatusBadRequest, "invalid_user")
		return
	}
	if err := s.store.RemoveMember(r.Context(), session, id, user); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) transferGroup(w http.ResponseWriter, r *http.Request, session store.Session, id uint64) {
	var input struct {
		UserID uint64 `json:"user_id"`
	}
	if !read(w, r, &input) {
		return
	}
	if input.UserID == 0 {
		problem(w, http.StatusBadRequest, "invalid_user")
		return
	}
	if err := s.store.TransferGroup(r.Context(), session, id, input.UserID); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
