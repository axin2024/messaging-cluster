package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"time"
)

type Message struct {
	ConversationID uint64    `json:"conversation_id"`
	Seq            uint64    `json:"seq"`
	SenderID       uint64    `json:"sender_id"`
	ClientID       string    `json:"client_id"`
	Body           string    `json:"body"`
	CreatedAt      time.Time `json:"created_at"`
}

type MessageReceipt struct {
	Message   Message `json:"message"`
	Duplicate bool    `json:"duplicate"`
}

const messageColumns = "m.conversation_id, m.seq, m.sender_id, m.client_id, m.body, m.created_at"

func (s *Store) SendMessage(ctx context.Context, session Session, connection *Connection, id uint64, clientID, body string) (MessageReceipt, error) {
	tx, err := s.beginConversationWrite(ctx, session)
	if err != nil {
		return MessageReceipt{}, err
	}
	defer tx.Rollback()
	if connection != nil {
		if err := checkMessageConnection(ctx, tx, session, *connection); err != nil {
			return MessageReceipt{}, err
		}
	}
	conversation, err := lockConversation(ctx, tx, session.Scope, id)
	if err != nil {
		return MessageReceipt{}, err
	}
	if _, err := activeMember(ctx, tx, id, session.UserID); err != nil {
		return MessageReceipt{}, err
	}
	hash := sha256.Sum256([]byte(body))
	var previousHash []byte
	previous, err := scanMessage(tx.QueryRowContext(ctx, `SELECT `+messageColumns+`, m.payload_hash FROM messages AS m
		WHERE m.sender_id = ? AND m.sender_device_id = ? AND m.client_id = ?`, session.UserID, session.DeviceID, clientID), &previousHash)
	if err == nil {
		if previous.ConversationID != id || !bytes.Equal(hash[:], previousHash) {
			return MessageReceipt{}, ErrPayloadConflict
		}
		if err := tx.Commit(); err != nil {
			return MessageReceipt{}, err
		}
		return MessageReceipt{Message: previous, Duplicate: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return MessageReceipt{}, err
	}
	seq := conversation.lastSeq + 1
	if _, err := tx.ExecContext(ctx, `INSERT INTO messages (tenant_id, app_id, conversation_id, seq, sender_id, sender_device_id, client_id, payload_hash, body)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, session.TenantID, session.AppID, id, seq, session.UserID, session.DeviceID, clientID, hash[:], body); err != nil {
		if errors.Is(conflict(err), ErrConflict) {
			return MessageReceipt{}, ErrPayloadConflict
		}
		return MessageReceipt{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO message_outbox (conversation_id, seq) VALUES (?, ?)`, id, seq); err != nil {
		return MessageReceipt{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE conversations SET last_seq = ? WHERE id = ?`, seq, id); err != nil {
		return MessageReceipt{}, err
	}
	message, err := scanMessage(tx.QueryRowContext(ctx, `SELECT `+messageColumns+` FROM messages AS m
		WHERE m.conversation_id = ? AND m.seq = ?`, id, seq), nil)
	if err != nil {
		return MessageReceipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return MessageReceipt{}, err
	}
	return MessageReceipt{Message: message}, nil
}

func (s *Store) MessageByClientID(ctx context.Context, session Session, id uint64, clientID string) (Message, error) {
	message, err := scanMessage(s.db.QueryRowContext(ctx, `SELECT `+messageColumns+` FROM messages AS m
		WHERE m.tenant_id = ? AND m.app_id = ? AND m.conversation_id = ? AND m.sender_id = ?
		AND m.sender_device_id = ? AND m.client_id = ?`, session.TenantID, session.AppID, id, session.UserID, session.DeviceID, clientID), nil)
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, ErrNotFound
	}
	return message, err
}

func checkMessageConnection(ctx context.Context, tx *sql.Tx, session Session, connection Connection) error {
	var valid uint64
	err := tx.QueryRowContext(ctx, `SELECT d.id FROM devices AS d
		JOIN device_connections AS c ON c.device_id = d.id AND c.epoch = d.connection_epoch
		JOIN gateway_nodes AS n ON n.id = c.node_id
		WHERE d.id = ? AND c.epoch = ? AND n.id = ? AND c.credential_version = ? AND c.disconnected_at IS NULL
			AND n.stopped_at IS NULL AND n.lease_until > UTC_TIMESTAMP(6) FOR SHARE`,
		session.DeviceID, connection.Epoch, connection.Node.ID, session.CredentialVersion).Scan(&valid)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrUnauthorized
	}
	return err
}

func scanMessage(row interface{ Scan(...any) error }, hash *[]byte) (Message, error) {
	var message Message
	fields := []any{&message.ConversationID, &message.Seq, &message.SenderID, &message.ClientID, &message.Body, &message.CreatedAt}
	if hash != nil {
		fields = append(fields, hash)
	}
	return message, row.Scan(fields...)
}

const membershipScope = `tenant_id = ? AND app_id = ? AND conversation_id = ? AND user_id = ?`

func messageReader(ctx context.Context, tx *sql.Tx, session Session, id uint64) error {
	var found int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM memberships WHERE `+membershipScope+` LIMIT 1`,
		session.TenantID, session.AppID, id, session.UserID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func (s *Store) Message(ctx context.Context, session Session, id, seq uint64) (Message, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return Message{}, err
	}
	defer tx.Rollback()
	if err := messageReader(ctx, tx, session, id); err != nil {
		return Message{}, err
	}
	var found int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM memberships WHERE `+membershipScope+`
		AND from_seq <= ? AND (to_seq IS NULL OR to_seq >= ?) LIMIT 1`, session.TenantID, session.AppID, id, session.UserID, seq, seq).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, ErrForbidden
	}
	if err != nil {
		return Message{}, err
	}
	message, err := scanMessage(tx.QueryRowContext(ctx, `SELECT `+messageColumns+` FROM messages AS m
		WHERE m.conversation_id = ? AND m.seq = ?`, id, seq), nil)
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, ErrNotFound
	}
	if err != nil {
		return Message{}, err
	}
	return message, tx.Commit()
}

func (s *Store) Messages(ctx context.Context, session Session, id, after uint64, limit int) ([]Message, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := messageReader(ctx, tx, session, id); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+messageColumns+` FROM messages AS m
		WHERE m.conversation_id = ? AND m.seq > ? AND EXISTS (SELECT 1 FROM memberships AS member
			WHERE member.conversation_id = m.conversation_id AND member.tenant_id = m.tenant_id AND member.app_id = m.app_id
			AND member.user_id = ? AND member.from_seq <= m.seq AND (member.to_seq IS NULL OR member.to_seq >= m.seq))
		ORDER BY m.seq LIMIT ?`, id, after, session.UserID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Message, 0)
	for rows.Next() {
		message, err := scanMessage(rows, nil)
		if err != nil {
			return nil, err
		}
		items = append(items, message)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, tx.Commit()
}
