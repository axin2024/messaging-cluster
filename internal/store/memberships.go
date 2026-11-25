package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type Membership struct {
	UserID   uint64    `json:"user_id"`
	Role     string    `json:"role"`
	FromSeq  uint64    `json:"from_seq"`
	JoinedAt time.Time `json:"joined_at"`
	id       uint64
}

func activeMember(ctx context.Context, tx *sql.Tx, conversation, user uint64) (Membership, error) {
	var member Membership
	err := tx.QueryRowContext(ctx, `SELECT id, user_id, role, from_seq, joined_at FROM memberships
		WHERE conversation_id = ? AND active_user_id = ?`, conversation, user).
		Scan(&member.id, &member.UserID, &member.Role, &member.FromSeq, &member.JoinedAt)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrForbidden
	}
	return member, err
}

func insertMember(ctx context.Context, tx *sql.Tx, scope Scope, conversation, user uint64, role string, from uint64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO memberships (tenant_id, app_id, conversation_id, user_id, role, from_seq)
		VALUES (?, ?, ?, ?, ?, ?)`, scope.TenantID, scope.AppID, conversation, user, role, from)
	return err
}

func lockGroup(ctx context.Context, tx *sql.Tx, session Session, id uint64) (Conversation, Membership, error) {
	conversation, err := lockConversation(ctx, tx, session.Scope, id)
	if err != nil {
		return Conversation{}, Membership{}, err
	}
	actor, err := activeMember(ctx, tx, conversation.ID, session.UserID)
	if err == nil && conversation.Kind != "group" {
		err = ErrForbidden
	}
	return conversation, actor, err
}

func (s *Store) SetMember(ctx context.Context, session Session, id, user uint64, role string) (Membership, error) {
	if role != "member" && role != "admin" {
		return Membership{}, ErrForbidden
	}
	tx, err := s.beginConversationWrite(ctx, session)
	if err != nil {
		return Membership{}, err
	}
	defer tx.Rollback()
	conversation, actor, err := lockGroup(ctx, tx, session, id)
	if err != nil {
		return Membership{}, err
	}
	if actor.Role == "member" || (actor.Role == "admin" && role != "member") {
		return Membership{}, ErrForbidden
	}
	if err := scopedUser(ctx, tx, session.Scope, user); err != nil {
		return Membership{}, err
	}
	target, err := activeMember(ctx, tx, id, user)
	switch {
	case errors.Is(err, ErrForbidden):
		from := uint64(1)
		if conversation.HistoryPolicy == "since_join" {
			from = conversation.lastSeq + 1
		}
		if err := insertMember(ctx, tx, session.Scope, id, user, role, from); err != nil {
			return Membership{}, err
		}
	case err != nil:
		return Membership{}, err
	case target.Role == "owner" || (actor.Role == "admin" && target.Role == "admin"):
		return Membership{}, ErrForbidden
	case target.Role != role:
		if err := changeRole(ctx, tx, target.id, role); err != nil {
			return Membership{}, err
		}
	}
	member, err := activeMember(ctx, tx, id, user)
	if err != nil {
		return Membership{}, err
	}
	return member, tx.Commit()
}

func (s *Store) RemoveMember(ctx context.Context, session Session, id, user uint64) error {
	tx, err := s.beginConversationWrite(ctx, session)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	conversation, actor, err := lockGroup(ctx, tx, session, id)
	if err != nil {
		return err
	}
	if user != actor.UserID && actor.Role == "member" {
		return ErrForbidden
	}
	target, err := activeMember(ctx, tx, id, user)
	if errors.Is(err, ErrForbidden) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if target.Role == "owner" || (target.Role == "admin" && target.UserID != actor.UserID && actor.Role != "owner") {
		return ErrForbidden
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memberships SET to_seq = ?, left_at = UTC_TIMESTAMP(6) WHERE id = ?`,
		conversation.lastSeq, target.id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) TransferGroup(ctx context.Context, session Session, id, user uint64) error {
	tx, err := s.beginConversationWrite(ctx, session)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, actor, err := lockGroup(ctx, tx, session, id)
	if err != nil {
		return err
	}
	if actor.Role != "owner" || actor.UserID == user {
		return ErrForbidden
	}
	target, err := activeMember(ctx, tx, id, user)
	if err != nil {
		return err
	}
	if err := changeRole(ctx, tx, actor.id, "admin"); err != nil {
		return err
	}
	if err := changeRole(ctx, tx, target.id, "owner"); err != nil {
		return err
	}
	return tx.Commit()
}

func changeRole(ctx context.Context, tx *sql.Tx, membershipID uint64, role string) error {
	_, err := tx.ExecContext(ctx, `UPDATE memberships SET role = ? WHERE id = ?`, role, membershipID)
	return err
}

func (s *Store) Members(ctx context.Context, session Session, id, after uint64) ([]Membership, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var actor uint64
	err = tx.QueryRowContext(ctx, `SELECT id FROM memberships WHERE tenant_id = ? AND app_id = ?
		AND conversation_id = ? AND active_user_id = ?`, session.TenantID, session.AppID, id, session.UserID).Scan(&actor)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT user_id, role, from_seq, joined_at FROM memberships
		WHERE conversation_id = ? AND active_user_id > ? ORDER BY active_user_id LIMIT 50`, id, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Membership, 0)
	for rows.Next() {
		var item Membership
		if err := rows.Scan(&item.UserID, &item.Role, &item.FromSeq, &item.JoinedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, tx.Commit()
}
