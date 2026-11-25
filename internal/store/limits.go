package store

import (
	"context"
	"database/sql"
)

const DefaultMessagesPerSecond = 250

func consumeSendQuota(ctx context.Context, tx *sql.Tx, tenant uint64) error {
	var second uint64
	var limit uint32
	if err := tx.QueryRowContext(ctx, `SELECT UNIX_TIMESTAMP(), COALESCE(
		(SELECT messages_per_second FROM tenant_message_limits WHERE tenant_id = ?), ?)`, tenant, DefaultMessagesPerSecond).
		Scan(&second, &limit); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO tenant_send_buckets (tenant_id, second_epoch, accepted)
		VALUES (?, ?, 1) ON DUPLICATE KEY UPDATE accepted = accepted + 1`, tenant, second); err != nil {
		return err
	}
	var used uint32
	if err := tx.QueryRowContext(ctx, `SELECT accepted FROM tenant_send_buckets WHERE tenant_id = ? AND second_epoch = ?`, tenant, second).
		Scan(&used); err != nil {
		return err
	}
	if used > limit {
		return ErrRateLimited
	}
	return nil
}

func (s *Store) SetTenantSendLimit(ctx context.Context, tenantKey string, limit int) error {
	var tenant uint64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM tenants WHERE tenant_key = ?`, tenantKey).Scan(&tenant)
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO tenant_message_limits (tenant_id, messages_per_second)
		VALUES (?, ?) ON DUPLICATE KEY UPDATE messages_per_second = ?`, tenant, limit, limit)
	return err
}

func (s *Store) PruneSendBuckets(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM tenant_send_buckets WHERE second_epoch < UNIX_TIMESTAMP() - 120 LIMIT 2000`)
	return err
}
