package store

import (
	"context"
	"database/sql"
	"errors"
)

type SharedPreflight struct {
	Tenant      string   `json:"tenant"`
	Schema      uint     `json:"schema_version"`
	Groups      uint64   `json:"ordinary_groups"`
	Messages    uint64   `json:"ordinary_group_messages"`
	Memberships uint64   `json:"ordinary_group_memberships"`
	Conflicts   []uint64 `json:"owner_conflict_group_ids"`
	Pending     []uint64 `json:"pending_membership_operation_ids"`
}

func (s *Store) SharedPreflight(ctx context.Context, tenant string) (SharedPreflight, error) {
	out := SharedPreflight{Tenant: tenant, Conflicts: []uint64{}, Pending: []uint64{}}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var dirty bool
	if err = tx.QueryRowContext(ctx, "SELECT version,dirty FROM schema_migrations").Scan(&out.Schema, &dirty); err != nil {
		return out, err
	}
	if out.Schema < 7 || dirty {
		return out, errors.New("shared preflight requires a clean schema at version 7 or later")
	}
	var tenantID uint64
	if err = tx.QueryRowContext(ctx, "SELECT id FROM tenants WHERE tenant_key=?", tenant).Scan(&tenantID); errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	} else if err != nil {
		return out, err
	}
	err = tx.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM conversations WHERE tenant_id=? AND kind='group'),
		(SELECT COUNT(*) FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE m.tenant_id=? AND c.kind='group'),
		(SELECT COUNT(*) FROM memberships m JOIN conversations c ON c.id=m.conversation_id WHERE m.tenant_id=? AND c.kind='group')`,
		tenantID, tenantID, tenantID).Scan(&out.Groups, &out.Messages, &out.Memberships)
	if err != nil {
		return out, err
	}
	out.Conflicts, err = preflightIDs(ctx, tx, `SELECT c.id FROM conversations c LEFT JOIN memberships m
		ON m.conversation_id=c.id AND m.left_at IS NULL AND m.role='owner'
		WHERE c.tenant_id=? AND c.kind='group' GROUP BY c.id HAVING COUNT(m.id)<>1 ORDER BY c.id`, tenantID)
	if err != nil {
		return out, err
	}
	out.Pending, err = preflightIDs(ctx, tx, `SELECT id FROM membership_operations
		WHERE tenant_id=? AND completed_at IS NULL ORDER BY id`, tenantID)
	if err != nil {
		return out, err
	}
	return out, tx.Commit()
}

func preflightIDs(ctx context.Context, tx *sql.Tx, statement string, tenant uint64) ([]uint64, error) {
	rows, err := tx.QueryContext(ctx, statement, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []uint64{}
	for rows.Next() {
		var id uint64
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) TenantSharedEnabled(ctx context.Context, tenant uint64) (bool, error) {
	var enabled bool
	err := s.db.QueryRowContext(ctx, "SELECT shared_enabled FROM tenants WHERE id=?", tenant).Scan(&enabled)
	return enabled, err
}

func (s *Store) SetTenantShared(ctx context.Context, tenant string, enabled bool) error {
	result, err := s.db.ExecContext(ctx, "UPDATE tenants SET shared_enabled=? WHERE tenant_key=?", enabled, tenant)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil || count > 0 {
		return err
	}
	var actual bool
	err = s.db.QueryRowContext(ctx, "SELECT shared_enabled FROM tenants WHERE tenant_key=?", tenant).Scan(&actual)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
