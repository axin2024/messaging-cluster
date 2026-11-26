package httpapi

import (
	"net/http"
	"strconv"

	"github.com/axin2024/messaging-cluster/internal/credential"
	"github.com/axin2024/messaging-cluster/internal/store"
)

func (s *Server) prepareMembershipBatch(w http.ResponseWriter, r *http.Request, session store.Session, id uint64) {
	var input struct {
		ClientID string                   `json:"client_id"`
		Items    []store.MembershipChange `json:"items"`
	}
	if !read(w, r, &input) {
		return
	}
	if !credential.ValidKey(input.ClientID) || !validMembershipChanges(input.Items) {
		problem(w, http.StatusBadRequest, "invalid_membership_batch")
		return
	}
	item, err := s.store.PrepareMembershipBatch(r.Context(), session, id, input.ClientID, input.Items)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, http.StatusOK, item)
}

func validMembershipChanges(items []store.MembershipChange) bool {
	if len(items) == 0 || len(items) > 30 {
		return false
	}
	seen := make(map[uint64]bool, len(items))
	for _, item := range items {
		if item.UserID == 0 || seen[item.UserID] {
			return false
		}
		seen[item.UserID] = true
		switch item.Action {
		case "set":
			if item.Role != "member" && item.Role != "admin" {
				return false
			}
		case "remove":
			if item.Role != "" {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func (s *Server) membershipBatch(w http.ResponseWriter, r *http.Request, session store.Session, id uint64) {
	clientID := r.PathValue("client_id")
	if !credential.ValidKey(clientID) {
		problem(w, http.StatusBadRequest, "invalid_client_id")
		return
	}
	item, err := s.store.MembershipBatch(r.Context(), session, id, clientID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, http.StatusOK, item)
}

func (s *Server) executeMembershipBatch(w http.ResponseWriter, r *http.Request, session store.Session, id uint64) {
	clientID := r.PathValue("client_id")
	if !credential.ValidKey(clientID) {
		problem(w, http.StatusBadRequest, "invalid_client_id")
		return
	}
	item, err := s.store.ExecuteMembershipBatch(r.Context(), session, id, clientID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, http.StatusOK, item)
}

func (s *Server) membershipOperations(w http.ResponseWriter, r *http.Request, session store.Session, id uint64) {
	var before uint64
	if cursor := r.URL.Query().Get("before"); cursor != "" {
		var err error
		before, err = strconv.ParseUint(cursor, 10, 64)
		if err != nil || before == 0 {
			problem(w, http.StatusBadRequest, "invalid_cursor")
			return
		}
	}
	items, err := s.store.MembershipOperations(r.Context(), session, id, before)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	response := struct {
		Items      []store.MembershipOperation `json:"items"`
		NextBefore uint64                      `json:"next_before,omitempty"`
	}{Items: items}
	if len(items) > 20 {
		response.Items = items[:20]
		response.NextBefore = response.Items[len(response.Items)-1].ID
	}
	write(w, http.StatusOK, response)
}
