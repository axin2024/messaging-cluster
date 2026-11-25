package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/axin2024/messaging-cluster/internal/credential"
	"github.com/axin2024/messaging-cluster/internal/store"
)

type sessionResponse struct {
	Token   string        `json:"token"`
	Session store.Session `json:"session"`
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.authAdmin(w, r)
	if !ok {
		return
	}
	var input struct {
		Username    string `json:"username"`
		DisplayName string `json:"display_name"`
		Password    string `json:"password"`
	}
	if !read(w, r, &input) {
		return
	}
	displayName := strings.TrimSpace(input.DisplayName)
	if !credential.ValidKey(input.Username) || !credential.ValidPassword(input.Password) ||
		displayName == "" || utf8.RuneCountInString(displayName) > 100 {
		problem(w, http.StatusUnprocessableEntity, "invalid_account_fields")
		return
	}
	if !s.workPassword(w) {
		return
	}
	hash, err := credential.HashPassword(input.Password)
	<-s.passwordJobs
	if err != nil {
		s.fail(w, r, err)
		return
	}
	user, err := s.store.CreateUser(r.Context(), scope, input.Username, displayName, hash)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, http.StatusCreated, user)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Tenant    string `json:"tenant"`
		App       string `json:"app"`
		Username  string `json:"username"`
		Password  string `json:"password"`
		DeviceKey string `json:"device_key"`
	}
	if !read(w, r, &input) {
		return
	}
	if !credential.ValidKey(input.Tenant) || !credential.ValidKey(input.App) || !credential.ValidKey(input.Username) ||
		!credential.ValidKey(input.DeviceKey) || !credential.ValidPassword(input.Password) {
		problem(w, http.StatusUnprocessableEntity, "invalid_session_fields")
		return
	}
	account, hash, err := s.store.Credentials(r.Context(), input.Tenant, input.App, input.Username)
	missing := errors.Is(err, store.ErrUnauthorized)
	if err != nil && !missing {
		s.fail(w, r, err)
		return
	}
	if missing {
		hash = s.dummyHash
	}
	if !s.workPassword(w) {
		return
	}
	valid := credential.VerifyPassword(hash, input.Password)
	<-s.passwordJobs
	if missing || !valid {
		s.fail(w, r, store.ErrUnauthorized)
		return
	}
	token, digest, err := credential.IssueToken()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	session, err := s.store.LoginDevice(r.Context(), account, input.DeviceKey, digest)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, http.StatusCreated, sessionResponse{Token: token, Session: session})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	session, ok := s.authSession(w, r)
	if ok {
		write(w, http.StatusOK, session)
	}
}

func (s *Server) listDevices(w http.ResponseWriter, r *http.Request) {
	session, ok := s.authSession(w, r)
	if !ok {
		return
	}
	var after uint64
	if cursor := r.URL.Query().Get("after"); cursor != "" {
		after, ok = parseID(cursor)
		if !ok {
			problem(w, http.StatusBadRequest, "invalid_cursor")
			return
		}
	}
	devices, err := s.store.ListDevices(r.Context(), session, after)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	response := struct {
		Items     []store.Device `json:"items"`
		NextAfter uint64         `json:"next_after,omitempty"`
	}{Items: devices}
	if len(devices) == 100 {
		response.NextAfter = devices[len(devices)-1].DeviceID
	}
	write(w, http.StatusOK, response)
}

func (s *Server) rotate(w http.ResponseWriter, r *http.Request) {
	session, ok := s.authSession(w, r)
	if !ok {
		return
	}
	token, digest, err := credential.IssueToken()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	session, err = s.store.Rotate(r.Context(), session, digest)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, http.StatusOK, sessionResponse{Token: token, Session: session})
}

func (s *Server) revoke(w http.ResponseWriter, r *http.Request) {
	session, ok := s.authSession(w, r)
	if !ok {
		return
	}
	target, ok := parseID(r.PathValue("id"))
	if !ok {
		problem(w, http.StatusBadRequest, "invalid_device")
		return
	}
	if err := s.store.Revoke(r.Context(), session, target); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
