package httpapi

import (
	"net/http"
	"strings"
	"time"

	"github.com/axin2024/messaging-cluster/internal/credential"
	"github.com/axin2024/messaging-cluster/internal/gateway"
	"github.com/axin2024/messaging-cluster/internal/store"
)

func (s *Server) gatewayTicket(w http.ResponseWriter, r *http.Request) {
	session, ok := s.authSession(w, r)
	if !ok {
		return
	}
	token, digest, err := credential.IssueToken()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.store.CreateGatewayTicket(r.Context(), session, digest); err != nil {
		s.fail(w, r, err)
		return
	}
	response := struct {
		Ticket    string `json:"ticket"`
		ExpiresIn int64  `json:"expires_in"`
	}{Ticket: token, ExpiresIn: int64(store.GatewayTicketTTL / time.Second)}
	write(w, http.StatusCreated, response)
}

func (s *Server) upgradeGateway(w http.ResponseWriter, r *http.Request) {
	if !headerOffers(r.Header.Values("Connection"), "upgrade") || !headerOffers(r.Header.Values("Upgrade"), "websocket") {
		w.Header().Set("Upgrade", "websocket")
		problem(w, http.StatusUpgradeRequired, "websocket_required")
		return
	}
	offers := r.Header.Values("Sec-WebSocket-Protocol")
	protocol := false
	var tickets []string
	for _, header := range offers {
		for item := range strings.SplitSeq(header, ",") {
			item = strings.TrimSpace(item)
			if item == gateway.Protocol {
				protocol = true
			} else if ticket, ok := strings.CutPrefix(item, "ticket."); ok {
				tickets = append(tickets, ticket)
			}
		}
	}
	if !protocol {
		problem(w, http.StatusBadRequest, "websocket_protocol_required")
		return
	}
	if len(tickets) == 0 {
		session, ok := s.authSession(w, r)
		if ok {
			s.gateway.Serve(w, r, session)
		}
		return
	}
	if len(tickets) != 1 || len(r.Header.Values("Authorization")) != 0 {
		problem(w, http.StatusBadRequest, "ambiguous_websocket_credential")
		return
	}
	digest, valid := credential.TokenDigest(tickets[0])
	if !valid {
		s.fail(w, r, store.ErrUnauthorized)
		return
	}
	session, err := s.store.ConsumeGatewayTicket(r.Context(), digest)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.gateway.Serve(w, r, session)
}

func headerOffers(lines []string, name string) bool {
	for _, line := range lines {
		for value := range strings.SplitSeq(line, ",") {
			if strings.EqualFold(strings.TrimSpace(value), name) {
				return true
			}
		}
	}
	return false
}

func (s *Server) presence(w http.ResponseWriter, r *http.Request) {
	session, ok := s.authSession(w, r)
	if !ok {
		return
	}
	deviceID, ok := parseID(r.PathValue("id"))
	if !ok {
		problem(w, http.StatusBadRequest, "invalid_device")
		return
	}
	state, err := s.store.Presence(r.Context(), session, deviceID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, http.StatusOK, state)
}

func (s *Server) connections(w http.ResponseWriter, r *http.Request) {
	session, ok := s.authSession(w, r)
	if !ok {
		return
	}
	deviceID, ok := parseID(r.PathValue("id"))
	if !ok {
		problem(w, http.StatusBadRequest, "invalid_device")
		return
	}
	var before uint64
	if cursor := r.URL.Query().Get("before"); cursor != "" {
		before, ok = parseID(cursor)
		if !ok {
			problem(w, http.StatusBadRequest, "invalid_cursor")
			return
		}
	}
	if _, err := s.store.Presence(r.Context(), session, deviceID); err != nil {
		s.fail(w, r, err)
		return
	}
	items, err := s.store.Connections(r.Context(), session, deviceID, before)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	response := struct {
		Items      []store.Connection `json:"items"`
		NextBefore uint64             `json:"next_before,omitempty"`
	}{Items: items}
	if len(items) == 50 {
		response.NextBefore = items[len(items)-1].Epoch
	}
	write(w, http.StatusOK, response)
}
