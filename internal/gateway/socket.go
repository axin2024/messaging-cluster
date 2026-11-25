package gateway

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/axin2024/messaging-cluster/internal/credential"
	"github.com/axin2024/messaging-cluster/internal/store"
)

func (g *Gateway) Serve(w http.ResponseWriter, r *http.Request, session store.Session) {
	if !g.reserve() {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "gateway unavailable", http.StatusServiceUnavailable)
		return
	}
	defer g.release()
	socket, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{Protocol}})
	if err != nil {
		return
	}
	defer socket.CloseNow()
	socket.SetReadLimit(32 << 10)

	ctx, cancel := context.WithCancelCause(context.WithoutCancel(r.Context()))
	defer cancel(nil)
	admit, cancelAdmit := context.WithTimeout(ctx, 2*time.Second)
	connection, err := g.store.Connect(admit, g.node, session)
	cancelAdmit()
	if err != nil {
		if errors.Is(err, store.ErrUnauthorized) {
			_ = socket.Close(websocket.StatusPolicyViolation, "device session ended")
		} else {
			g.logger.Warn("could not admit gateway connection", "node", g.node.ID, "error", err)
			_ = socket.Close(websocket.StatusTryAgainLater, "gateway unavailable")
		}
		return
	}
	client := &peer{connection: connection, cancel: cancel}
	reason := "transport_error"
	defer func() {
		_ = socket.CloseNow()
		g.remove(client)
		cleanup, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancelCleanup()
		if err := g.store.Disconnect(cleanup, connection, reason); err != nil {
			g.logger.Warn("could not record connection close", "node", g.node.ID, "device", connection.DeviceID, "error", err)
		}
	}()
	if err := g.add(client); err != nil {
		reason = closeReason(err)
		return
	}
	ready := struct {
		Type string `json:"type"`
		store.Connection
	}{Type: "ready", Connection: connection}
	if err := send(ctx, socket, ready); err != nil {
		if cause := context.Cause(ctx); cause != nil {
			reason = closeReason(cause)
		}
		return
	}

	pingDone := make(chan struct{})
	go func() {
		defer close(pingDone)
		g.ping(ctx, cancel, socket)
	}()
	reason = g.receive(ctx, socket, session, connection)
	cancel(nil)
	<-pingDone
}

func (g *Gateway) ping(ctx context.Context, cancel context.CancelCauseFunc, socket *websocket.Conn) {
	ticker := time.NewTicker(g.config.PingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		probe, cancelProbe := context.WithTimeout(ctx, g.config.PingTimeout)
		err := socket.Ping(probe)
		cancelProbe()
		if err != nil {
			cancel(errHeartbeat)
			return
		}
	}
}

func (g *Gateway) receive(ctx context.Context, socket *websocket.Conn, session store.Session, connection store.Connection) string {
	for {
		var request struct {
			Type           string `json:"type"`
			RequestID      string `json:"request_id,omitempty"`
			ConversationID uint64 `json:"conversation_id,omitempty"`
			store.OutgoingMessage
		}
		if err := wsjson.Read(ctx, socket, &request); err != nil {
			return readCloseReason(ctx, err)
		}
		response := struct {
			Type      string                `json:"type"`
			RequestID string                `json:"request_id,omitempty"`
			Error     string                `json:"error,omitempty"`
			Receipt   *store.MessageReceipt `json:"receipt,omitempty"`
		}{RequestID: request.RequestID}
		switch request.Type {
		case "ping":
			response.Type = "pong"
		case "message.send":
			if request.ConversationID == 0 || !credential.ValidMessage(request.ClientID, request.Body, request.AttachmentIDs) {
				response.Type, response.Error = "error", "invalid_message"
				break
			}
			operation, cancel := context.WithTimeout(ctx, 3*time.Second)
			receipt, err := g.store.SendMessage(operation, session, &connection, request.ConversationID, request.OutgoingMessage)
			cancel()
			if errors.Is(err, store.ErrUnauthorized) {
				return "authority_ended"
			}
			if err != nil {
				response.Type, response.Error = "error", g.messageError(err, connection)
				break
			}
			response.Type, response.Receipt = "message.accepted", &receipt
		default:
			response.Type, response.Error = "error", "invalid_frame_type"
		}
		if err := send(ctx, socket, response); err != nil {
			return readCloseReason(ctx, err)
		}
	}
}

func (g *Gateway) messageError(err error, connection store.Connection) string {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return "not_found"
	case errors.Is(err, store.ErrForbidden):
		return "forbidden"
	case errors.Is(err, store.ErrPayloadConflict):
		return "client_id_payload_conflict"
	case errors.Is(err, store.ErrInvalidAttachment):
		return "attachment_unavailable"
	default:
		g.logger.Warn("could not confirm message", "node", connection.Node.ID, "device", connection.DeviceID, "error", err)
		return "message_unconfirmed"
	}
}

func send(ctx context.Context, socket *websocket.Conn, response any) error {
	write, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return wsjson.Write(write, socket, response)
}

func readCloseReason(ctx context.Context, err error) string {
	if cause := context.Cause(ctx); cause != nil {
		return closeReason(cause)
	}
	switch websocket.CloseStatus(err) {
	case websocket.StatusNormalClosure, websocket.StatusGoingAway:
		return "client_closed"
	case websocket.StatusMessageTooBig, websocket.StatusInvalidFramePayloadData:
		return "protocol_error"
	default:
		return "transport_error"
	}
}

func closeReason(err error) string {
	switch err {
	case errSuperseded:
		return "superseded"
	case errInvalid:
		return "authority_ended"
	case errShutdown:
		return "node_shutdown"
	case errHeartbeat:
		return "heartbeat_timeout"
	default:
		return "transport_error"
	}
}
