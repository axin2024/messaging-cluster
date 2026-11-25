package httpapi

import (
	"net/http"
	"strconv"

	"github.com/axin2024/messaging-cluster/internal/credential"
	"github.com/axin2024/messaging-cluster/internal/store"
)

func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request, session store.Session, id uint64) {
	var input store.OutgoingMessage
	if !read(w, r, &input) {
		return
	}
	if !credential.ValidMessage(input.ClientID, input.Body, input.AttachmentIDs) {
		problem(w, http.StatusBadRequest, "invalid_message")
		return
	}
	receipt, err := s.store.SendMessage(r.Context(), session, nil, id, input)
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
	s.syncPage(w, r, session, id, &after)
}

func (s *Server) syncPage(w http.ResponseWriter, r *http.Request, session store.Session, id uint64, after *uint64) {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		var err error
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 {
			problem(w, http.StatusBadRequest, "invalid_limit")
			return
		}
	}
	result, err := s.store.Sync(r.Context(), session, id, after, limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, http.StatusOK, result)
}
