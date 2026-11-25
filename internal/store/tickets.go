package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const GatewayTicketTTL = 30 * time.Second

func (s *Store) CreateGatewayTicket(ctx context.Context, session Session, digest [32]byte) error {
	const query = `
		INSERT INTO gateway_tickets (token_digest, tenant_id, app_id, device_id, credential_version, expires_at)
		SELECT ?, tenant_id, app_id, id, credential_version, TIMESTAMPADD(MICROSECOND, ?, UTC_TIMESTAMP(6))
		FROM devices
		WHERE tenant_id = ? AND app_id = ? AND user_id = ? AND id = ? AND credential_version = ?
			AND revoked_at IS NULL AND expires_at > UTC_TIMESTAMP(6)`
	result, err := s.db.ExecContext(ctx, query, digest[:], GatewayTicketTTL.Microseconds(), session.TenantID, session.AppID,
		session.UserID, session.DeviceID, session.CredentialVersion)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err == nil && changed != 1 {
		return ErrUnauthorized
	}
	return err
}

func (s *Store) ConsumeGatewayTicket(ctx context.Context, digest [32]byte) (Session, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, err
	}
	defer tx.Rollback()
	const readTicket = `
		SELECT tenant_id, app_id, device_id, credential_version FROM gateway_tickets
		WHERE token_digest = ? AND expires_at > UTC_TIMESTAMP(6) FOR UPDATE`
	var session Session
	err = tx.QueryRowContext(ctx, readTicket, digest[:]).Scan(&session.TenantID, &session.AppID, &session.DeviceID, &session.CredentialVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrUnauthorized
	}
	if err != nil {
		return Session{}, err
	}
	const checkDevice = `
		SELECT d.user_id, u.username, u.display_name, d.device_key, d.expires_at
		FROM devices d JOIN users u ON u.tenant_id = d.tenant_id AND u.app_id = d.app_id AND u.id = d.user_id
		WHERE d.tenant_id = ? AND d.app_id = ? AND d.id = ? AND d.credential_version = ?
			AND d.revoked_at IS NULL AND d.expires_at > UTC_TIMESTAMP(6)`
	err = tx.QueryRowContext(ctx, checkDevice, session.TenantID, session.AppID, session.DeviceID, session.CredentialVersion).
		Scan(&session.UserID, &session.Username, &session.DisplayName, &session.DeviceKey, &session.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrUnauthorized
	}
	if err != nil {
		return Session{}, err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM gateway_tickets WHERE token_digest = ?", digest[:]); err != nil {
		return Session{}, err
	}
	return session, tx.Commit()
}

func (s *Store) PruneGatewayTickets(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM gateway_tickets WHERE expires_at <= UTC_TIMESTAMP(6) ORDER BY expires_at LIMIT 1000")
	return err
}
