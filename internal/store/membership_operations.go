package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"
)

type MembershipChange struct {
	UserID uint64 `json:"user_id"`
	Action string `json:"action"`
	Role   string `json:"role,omitempty"`
}

type MembershipOutcome struct {
	MembershipChange
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

type MembershipOperation struct {
	ID             uint64              `json:"id"`
	ConversationID uint64              `json:"conversation_id"`
	ActorID        uint64              `json:"actor_id"`
	DeviceID       uint64              `json:"device_id"`
	ActorRole      string              `json:"actor_role"`
	ClientID       string              `json:"client_id,omitempty"`
	Kind           string              `json:"kind"`
	Version        int                 `json:"version"`
	Items          []MembershipOutcome `json:"items"`
	CreatedAt      time.Time           `json:"created_at"`
	CompletedAt    *time.Time          `json:"completed_at,omitempty"`
}

const membershipOperationColumns = `id, conversation_id, actor_id, device_id, actor_role,
	COALESCE(client_id, ''), kind, items, created_at, completed_at`

const membershipOperationScope = `tenant_id = ? AND app_id = ? AND conversation_id = ?`

func scanMembershipOperation(row interface{ Scan(...any) error }) (MembershipOperation, error) {
	var operation MembershipOperation
	var items []byte
	err := row.Scan(&operation.ID, &operation.ConversationID, &operation.ActorID, &operation.DeviceID,
		&operation.ActorRole, &operation.ClientID, &operation.Kind, &items, &operation.CreatedAt, &operation.CompletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return operation, ErrNotFound
	}
	if err != nil {
		return operation, err
	}
	operation.Version = 1
	if operation.Kind == "batch" && operation.CompletedAt != nil {
		operation.Version = 2
	}
	return operation, json.Unmarshal(items, &operation.Items)
}

func recordMembershipChange(ctx context.Context, tx *sql.Tx, session Session, conversation Conversation, actor Membership, change MembershipChange) error {
	items, err := json.Marshal([]MembershipOutcome{{MembershipChange: change, Status: "succeeded"}})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO membership_operations
		(tenant_id, app_id, conversation_id, actor_id, device_id, actor_role, kind, items, completed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, UTC_TIMESTAMP(6))`,
		session.TenantID, session.AppID, conversation.ID, actor.UserID, session.DeviceID, actor.Role, change.Action, items)
	return err
}

func findMembershipBatch(ctx context.Context, tx *sql.Tx, session Session, id uint64, clientID string) (MembershipOperation, error) {
	return scanMembershipOperation(tx.QueryRowContext(ctx, `SELECT `+membershipOperationColumns+`
		FROM membership_operations WHERE `+membershipOperationScope+` AND actor_id = ? AND client_id = ?`,
		session.TenantID, session.AppID, id, session.UserID, clientID))
}

func lockManagedGroup(ctx context.Context, tx *sql.Tx, session Session, id uint64) (Conversation, Membership, error) {
	conversation, actor, err := lockGroup(ctx, tx, session, id)
	if err == nil && actor.Role == "member" {
		err = ErrForbidden
	}
	return conversation, actor, err
}

func (s *Store) PrepareMembershipBatch(ctx context.Context, session Session, id uint64, clientID string, changes []MembershipChange) (MembershipOperation, error) {
	tx, err := s.beginConversationWrite(ctx, session)
	if err != nil {
		return MembershipOperation{}, err
	}
	defer tx.Rollback()
	_, actor, err := lockManagedGroup(ctx, tx, session, id)
	if err != nil {
		return MembershipOperation{}, err
	}
	previous, err := findMembershipBatch(ctx, tx, session, id, clientID)
	if err == nil {
		if !slices.EqualFunc(changes, previous.Items, func(change MembershipChange, item MembershipOutcome) bool {
			return change == item.MembershipChange
		}) {
			return MembershipOperation{}, ErrPayloadConflict
		}
		return previous, tx.Commit()
	}
	if !errors.Is(err, ErrNotFound) {
		return MembershipOperation{}, err
	}
	items := make([]MembershipOutcome, len(changes))
	for index, change := range changes {
		items[index] = MembershipOutcome{MembershipChange: change, Status: "not_started"}
	}
	encoded, err := json.Marshal(items)
	if err != nil {
		return MembershipOperation{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO membership_operations
		(tenant_id, app_id, conversation_id, actor_id, device_id, actor_role, client_id, kind, items)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'batch', ?)`,
		session.TenantID, session.AppID, id, actor.UserID, session.DeviceID, actor.Role, clientID, encoded)
	if err != nil {
		return MembershipOperation{}, err
	}
	operation, err := findMembershipBatch(ctx, tx, session, id, clientID)
	if err != nil {
		return MembershipOperation{}, err
	}
	return operation, tx.Commit()
}

func (s *Store) ExecuteMembershipBatch(ctx context.Context, session Session, id uint64, clientID string) (MembershipOperation, error) {
	tx, err := s.beginConversationWrite(ctx, session)
	if err != nil {
		return MembershipOperation{}, err
	}
	defer tx.Rollback()
	conversation, actor, err := lockManagedGroup(ctx, tx, session, id)
	if err != nil {
		return MembershipOperation{}, err
	}
	operation, err := findMembershipBatch(ctx, tx, session, id, clientID)
	if err != nil {
		return MembershipOperation{}, err
	}
	if operation.CompletedAt != nil {
		return operation, tx.Commit()
	}
	for index, item := range operation.Items {
		err := applyMembershipChange(ctx, tx, session.Scope, conversation, actor, item.MembershipChange)
		switch {
		case err == nil:
			item.Status = "succeeded"
		case errors.Is(err, ErrNotFound):
			item.Status, item.Reason = "failed", "not_found"
		case errors.Is(err, ErrForbidden):
			item.Status, item.Reason = "failed", "forbidden"
		default:
			return MembershipOperation{}, err
		}
		operation.Items[index] = item
	}
	encoded, err := json.Marshal(operation.Items)
	if err != nil {
		return MembershipOperation{}, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE membership_operations SET items = ?, actor_role = ?, device_id = ?,
		completed_at = UTC_TIMESTAMP(6) WHERE id = ?`, encoded, actor.Role, session.DeviceID, operation.ID)
	if err != nil {
		return MembershipOperation{}, err
	}
	operation, err = findMembershipBatch(ctx, tx, session, id, clientID)
	if err != nil {
		return MembershipOperation{}, err
	}
	return operation, tx.Commit()
}

func applyMembershipChange(ctx context.Context, tx *sql.Tx, scope Scope, conversation Conversation, actor Membership, change MembershipChange) error {
	if change.UserID == actor.UserID {
		return ErrForbidden
	}
	switch change.Action {
	case "set":
		_, err := setMember(ctx, tx, scope, conversation, actor, change.UserID, change.Role)
		return err
	case "remove":
		return removeMember(ctx, tx, conversation, actor, change.UserID)
	default:
		return ErrForbidden
	}
}

func (s *Store) beginMembershipOperationRead(ctx context.Context, session Session, id uint64) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	var member uint64
	err = tx.QueryRowContext(ctx, `SELECT m.id FROM memberships AS m JOIN conversations AS c ON c.id = m.conversation_id
		WHERE m.tenant_id = ? AND m.app_id = ? AND m.conversation_id = ? AND m.active_user_id = ?
		AND c.kind = 'group' AND m.role IN ('owner', 'admin')`, session.TenantID, session.AppID, id, session.UserID).Scan(&member)
	if err != nil {
		_ = tx.Rollback()
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrNotFound
		}
		return nil, err
	}
	return tx, nil
}

func (s *Store) MembershipBatch(ctx context.Context, session Session, id uint64, clientID string) (MembershipOperation, error) {
	tx, err := s.beginMembershipOperationRead(ctx, session, id)
	if err != nil {
		return MembershipOperation{}, err
	}
	defer tx.Rollback()
	item, err := findMembershipBatch(ctx, tx, session, id, clientID)
	if err != nil {
		return MembershipOperation{}, err
	}
	return item, tx.Commit()
}

func (s *Store) MembershipOperations(ctx context.Context, session Session, id, before uint64) ([]MembershipOperation, error) {
	tx, err := s.beginMembershipOperationRead(ctx, session, id)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	filter := ` ORDER BY id DESC LIMIT 21`
	args := []any{session.TenantID, session.AppID, id}
	if before > 0 {
		filter = ` AND id < ?` + filter
		args = append(args, before)
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+membershipOperationColumns+` FROM membership_operations WHERE `+membershipOperationScope+filter, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]MembershipOperation, 0)
	for rows.Next() {
		item, err := scanMembershipOperation(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, tx.Commit()
}
