package httpapi

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/axin2024/messaging-cluster/internal/credential"
	"github.com/axin2024/messaging-cluster/internal/store"
)

func (s *Server) incomingInvitations(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.authSharedAdmin(w, r)
	if !ok {
		return
	}
	var after uint64
	if cursor := r.URL.Query().Get("after"); cursor != "" {
		var err error
		after, err = strconv.ParseUint(cursor, 10, 64)
		if err != nil {
			problem(w, 422, "invalid_after")
			return
		}
	}
	items, err := s.store.IncomingInvitations(r.Context(), scope, after)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, 200, map[string]any{"items": items})
}

func (s *Server) changeSharedGrant(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.authSharedAdmin(w, r)
	if !ok {
		return
	}
	id, valid := parseID(r.PathValue("link"))
	key := r.Header.Get("Idempotency-Key")
	var change store.GrantChange
	if !read(w, r, &change) {
		return
	}
	change.ExpiresAt = change.ExpiresAt.UTC()
	if !valid || !credential.ValidKey(key) || change.UserID == 0 || (!change.Revoke && (!credential.ValidKey(change.Role) || change.ExpiresAt.IsZero() || change.ExpiresAt.Nanosecond()%1000 != 0)) {
		problem(w, 422, "invalid_grant")
		return
	}
	result, err := s.store.ChangeSharedGrant(r.Context(), admin, id, key, change)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, 200, result)
}

func (s *Server) changePartnerState(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.authSharedAdmin(w, r)
	if !ok {
		return
	}
	id, valid := parseID(r.PathValue("link"))
	var body struct {
		Epoch uint64 `json:"grant_epoch"`
		State string `json:"state"`
	}
	if !read(w, r, &body) {
		return
	}
	if !valid || body.Epoch == 0 || (body.State != "closed" && body.State != "active" && body.State != "frozen") {
		problem(w, 422, "invalid_link_state")
		return
	}
	result, err := s.store.ChangePartnerState(r.Context(), scope, id, body.Epoch, body.State)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, 200, result)
}

func (s *Server) sharedAudit(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.authSharedAdmin(w, r)
	if !ok {
		return
	}
	link, valid := parseID(r.PathValue("link"))
	var after uint64
	if value := r.URL.Query().Get("after"); value != "" {
		var err error
		after, err = strconv.ParseUint(value, 10, 64)
		valid = valid && err == nil
	}
	if !valid {
		problem(w, 422, "invalid_audit_page")
		return
	}
	items, err := s.store.SharedAudit(r.Context(), scope, link, after)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, 200, map[string]any{"items": items})
}

func (s *Server) createSharedSpace(w http.ResponseWriter, r *http.Request) {
	host, ok := s.authSharedAdmin(w, r)
	if !ok {
		return
	}
	var input struct {
		Key     string `json:"key"`
		Title   string `json:"title"`
		Purpose string `json:"purpose"`
	}
	if !read(w, r, &input) {
		return
	}
	input.Title = strings.TrimSpace(input.Title)
	input.Purpose = strings.TrimSpace(input.Purpose)
	if !credential.ValidKey(input.Key) || input.Title == "" || utf8.RuneCountInString(input.Title) > 160 || input.Purpose == "" || utf8.RuneCountInString(input.Purpose) > 600 {
		problem(w, 422, "invalid_space_fields")
		return
	}
	space, err := s.store.CreateSpace(r.Context(), host, input.Key, input.Title, input.Purpose)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, 201, space)
}

func normalizedPermissions(items []string) bool {
	slices.Sort(items)
	if len(items) < 1 || len(items) > 2 || !slices.Contains(items, "project:read") {
		return false
	}
	return len(items) == 1 || items[1] == "thread:write"
}

func (s *Server) invitePartner(w http.ResponseWriter, r *http.Request) {
	host, ok := s.authSharedAdmin(w, r)
	if !ok {
		return
	}
	space, valid := parseID(r.PathValue("space"))
	key := r.Header.Get("Idempotency-Key")
	var offer store.Offer
	if !read(w, r, &offer) {
		return
	}
	offer.AcceptBefore = offer.AcceptBefore.UTC()
	offer.ProjectEndsAt = offer.ProjectEndsAt.UTC()
	if !valid || !credential.ValidKey(key) || !credential.ValidKey(offer.PeerTenant) || !credential.ValidKey(offer.PeerApp) ||
		!normalizedPermissions(offer.Scope) || offer.SeatLimit < 1 || offer.SeatLimit > 10000 || offer.RetentionDays < 0 || offer.RetentionDays > 3650 || offer.AcceptBefore.IsZero() ||
		offer.ProjectEndsAt.Before(offer.AcceptBefore) || offer.AcceptBefore.Nanosecond()%1000 != 0 || offer.ProjectEndsAt.Nanosecond()%1000 != 0 {
		problem(w, 422, "invalid_invitation")
		return
	}
	invite, err := s.store.InvitePartner(r.Context(), host, space, key, offer)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, 201, invite)
}

func (s *Server) acceptPartner(w http.ResponseWriter, r *http.Request) {
	peer, ok := s.authSharedAdmin(w, r)
	if !ok {
		return
	}
	space, valid := parseID(r.PathValue("space"))
	org, validOrg := parseID(r.PathValue("org"))
	if !valid || !validOrg || peer.TenantID != org {
		s.fail(w, r, store.ErrNotFound)
		return
	}
	var input struct {
		Invitation    uint64   `json:"invitation_id"`
		Rev           uint64   `json:"policy_rev"`
		Permissions   []string `json:"scope"`
		History       bool     `json:"share_history"`
		RetentionDays int      `json:"retention_days"`
	}
	if !read(w, r, &input) {
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if input.Invitation == 0 || input.Rev == 0 || input.RetentionDays < 0 || input.RetentionDays > 3650 || !credential.ValidKey(key) || !normalizedPermissions(input.Permissions) {
		problem(w, 422, "invalid_acceptance")
		return
	}
	link, err := s.store.AcceptPartner(r.Context(), peer, space, input.Invitation, input.Rev, key, input.Permissions, input.History, input.RetentionDays)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, 200, link)
}
