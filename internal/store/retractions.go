package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type Retraction struct {
	ID             uint64    `json:"id"`
	ConversationID uint64    `json:"conversation_id"`
	Seq            uint64    `json:"seq"`
	ActorID        uint64    `json:"actor_id"`
	At             time.Time `json:"retracted_at"`
}

func (s *Store) Retract(ctx context.Context, session Session, id, seq uint64) (Retraction, error) {
	tx, err := s.beginConversationWrite(ctx, session)
	if err != nil {
		return Retraction{}, err
	}
	defer tx.Rollback()
	conversation, err := lockConversation(ctx, tx, session.Scope, id)
	if err != nil {
		return Retraction{}, err
	}
	member, err := activeMember(ctx, tx, id, session.UserID)
	if err != nil {
		return Retraction{}, err
	}
	if err := checkVisibleSequence(ctx, tx, session, id, seq); err != nil {
		return Retraction{}, err
	}
	var sender uint64
	var expired, retracted bool
	err = tx.QueryRowContext(ctx, `SELECT sender_id, expired_at IS NOT NULL, retracted_at IS NOT NULL
		FROM messages WHERE conversation_id = ? AND seq = ?`, id, seq).Scan(&sender, &expired, &retracted)
	if errors.Is(err, sql.ErrNoRows) {
		return Retraction{}, ErrNotFound
	}
	if err != nil {
		return Retraction{}, err
	}
	if sender != session.UserID && (conversation.Kind != "group" || member.Role == "member") {
		return Retraction{}, ErrForbidden
	}
	if expired {
		return Retraction{}, ErrHistoryExpired
	}
	if !retracted {
		if _, err := tx.ExecContext(ctx, `INSERT INTO message_retractions (tenant_id, app_id, conversation_id, seq, actor_id)
			VALUES (?, ?, ?, ?, ?)`, session.TenantID, session.AppID, id, seq, session.UserID); err != nil {
			return Retraction{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE messages AS m JOIN message_retractions AS r ON r.conversation_id = m.conversation_id AND r.seq = m.seq
			SET m.retracted_at = r.retracted_at, m.body = '' WHERE m.conversation_id = ? AND m.seq = ?`, id, seq); err != nil {
			return Retraction{}, err
		}
	}
	record, err := scanRetraction(tx.QueryRowContext(ctx, `SELECT id, conversation_id, seq, actor_id, retracted_at
		FROM message_retractions WHERE conversation_id = ? AND seq = ?`, id, seq))
	if err != nil {
		return Retraction{}, err
	}
	return record, tx.Commit()
}

func (s *Store) Retractions(ctx context.Context, session Session, id, after uint64) ([]Retraction, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := messageReader(ctx, tx, session, id); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT m.id, m.conversation_id, m.seq, m.actor_id, m.retracted_at FROM message_retractions AS m
		WHERE m.tenant_id = ? AND m.app_id = ? AND m.conversation_id = ? AND m.id > ? AND `+messageVisibleToUser+`
		ORDER BY m.id LIMIT 50`, session.TenantID, session.AppID, id, after, session.UserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Retraction, 0)
	for rows.Next() {
		record, err := scanRetraction(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, tx.Commit()
}

func scanRetraction(row interface{ Scan(...any) error }) (Retraction, error) {
	var record Retraction
	err := row.Scan(&record.ID, &record.ConversationID, &record.Seq, &record.ActorID, &record.At)
	return record, err
}
