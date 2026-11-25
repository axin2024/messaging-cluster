package httpapi

import (
	"net/http"
	"time"

	"github.com/axin2024/messaging-cluster/internal/store"
)

func (s *Server) inbox(w http.ResponseWriter, r *http.Request) {
	session, ok := s.authSession(w, r)
	if !ok {
		return
	}
	after, ok := afterCursor(w, r)
	if !ok {
		return
	}
	items, err := s.store.Inbox(r.Context(), session, after)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	result := page[store.InboxEntry]{Items: items}
	if len(items) == 50 {
		result.NextAfter = items[len(items)-1].ConversationID
	}
	write(w, http.StatusOK, result)
}

func (s *Server) sync(w http.ResponseWriter, r *http.Request, session store.Session, id uint64) {
	if r.URL.Query().Has("after") {
		after, ok := afterCursor(w, r)
		if ok {
			s.syncPage(w, r, session, id, &after)
		}
		return
	}
	s.syncPage(w, r, session, id, nil)
}

func position(w http.ResponseWriter, r *http.Request) (uint64, bool) {
	var input struct {
		Seq uint64 `json:"seq"`
	}
	if !read(w, r, &input) {
		return 0, false
	}
	if input.Seq == 0 {
		problem(w, http.StatusBadRequest, "invalid_sequence")
		return 0, false
	}
	return input.Seq, true
}

func (s *Server) acknowledgeSync(w http.ResponseWriter, r *http.Request, session store.Session, id uint64) {
	seq, ok := position(w, r)
	if !ok {
		return
	}
	synced, err := s.store.AcknowledgeSync(r.Context(), session, id, seq)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, http.StatusOK, map[string]uint64{"sync_seq": synced})
}

func (s *Server) readState(w http.ResponseWriter, r *http.Request, session store.Session, id uint64) {
	state, err := s.store.ReadState(r.Context(), session, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, http.StatusOK, state)
}

func (s *Server) markRead(w http.ResponseWriter, r *http.Request, session store.Session, id uint64) {
	seq, ok := position(w, r)
	if !ok {
		return
	}
	if err := s.store.MarkRead(r.Context(), session, id, seq); err != nil {
		s.fail(w, r, err)
		return
	}
	s.readState(w, r, session, id)
}

func (s *Server) adminConversationRoute(handler func(http.ResponseWriter, *http.Request, store.Scope, uint64)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := s.authAdmin(w, r)
		if !ok {
			return
		}
		id, ok := parseID(r.PathValue("id"))
		if !ok {
			problem(w, http.StatusBadRequest, "invalid_conversation")
			return
		}
		handler(w, r, scope, id)
	}
}

func (s *Server) expireHistory(w http.ResponseWriter, r *http.Request, scope store.Scope, id uint64) {
	var input struct {
		Before time.Time `json:"before"`
	}
	if !read(w, r, &input) {
		return
	}
	before := input.Before.UTC()
	if before.Year() < 1000 || before.After(time.Now()) {
		problem(w, http.StatusBadRequest, "invalid_retention_cutoff")
		return
	}
	result, err := s.store.ExpireHistory(r.Context(), scope, id, before)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, http.StatusOK, result)
}

func (s *Server) retentionRuns(w http.ResponseWriter, r *http.Request, scope store.Scope, id uint64) {
	after, ok := afterCursor(w, r)
	if !ok {
		return
	}
	items, err := s.store.RetentionRuns(r.Context(), scope, id, after)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	result := page[store.RetentionRun]{Items: items}
	if len(items) == 50 {
		result.NextAfter = items[len(items)-1].ID
	}
	write(w, http.StatusOK, result)
}
