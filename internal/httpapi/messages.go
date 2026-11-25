package httpapi

import (
	"net/http"
	"strconv"

	"github.com/axin2024/messaging-cluster/internal/credential"
	"github.com/axin2024/messaging-cluster/internal/store"
)

func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request, session store.Session, id uint64) {
	var input struct {
		ClientID string `json:"client_id"`
		Body     string `json:"body"`
	}
	if !read(w, r, &input) {
		return
	}
	if !credential.ValidMessage(input.ClientID, input.Body) {
		problem(w, http.StatusBadRequest, "invalid_message")
		return
	}
	receipt, err := s.store.SendMessage(r.Context(), session, nil, id, input.ClientID, input.Body)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	status := http.StatusCreated
	if receipt.Duplicate {
		status = http.StatusOK
	}
	write(w, status, receipt)
}

func (s *Server) getMessage(w http.ResponseWriter, r *http.Request, session store.Session, id uint64) {
	seq, ok := parseID(r.PathValue("seq"))
	if !ok {
		problem(w, http.StatusBadRequest, "invalid_sequence")
		return
	}
	item, err := s.store.Message(r.Context(), session, id, seq)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, http.StatusOK, item)
}

func (s *Server) getMessageByClientID(w http.ResponseWriter, r *http.Request, session store.Session, id uint64) {
	clientID := r.PathValue("client_id")
	if !credential.ValidKey(clientID) {
		problem(w, http.StatusBadRequest, "invalid_client_id")
		return
	}
	item, err := s.store.MessageByClientID(r.Context(), session, id, clientID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, http.StatusOK, item)
}

func (s *Server) listMessages(w http.ResponseWriter, r *http.Request, session store.Session, id uint64) {
	after, ok := afterCursor(w, r)
	if !ok {
		return
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		var err error
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 {
			problem(w, http.StatusBadRequest, "invalid_limit")
			return
		}
	}
	items, err := s.store.Messages(r.Context(), session, id, after, limit+1)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	more := len(items) > limit
	if more {
		items = items[:limit]
	}
	if len(items) > 0 {
		after = items[len(items)-1].Seq
	}
	write(w, http.StatusOK, struct {
		Items     []store.Message `json:"items"`
		NextAfter uint64          `json:"next_after"`
		HasMore   bool            `json:"has_more"`
	}{Items: items, NextAfter: after, HasMore: more})
}
