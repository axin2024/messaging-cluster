package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type Scope struct {
	TenantID uint64 `json:"tenant_id"`
	AppID    uint64 `json:"app_id"`
}

type Account struct {
	Scope
	UserID      uint64 `json:"user_id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
}

type Device struct {
	DeviceID  uint64     `json:"device_id"`
	DeviceKey string     `json:"device_key"`
	ExpiresAt time.Time  `json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

type Session struct {
	Account
	Device
	CredentialVersion uint64 `json:"-"`
}

func (s *Store) CreateApp(ctx context.Context, tenantKey, appKey string, digest [32]byte) (Scope, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Scope{}, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, "INSERT INTO tenants (tenant_key,shared_enabled) VALUES (?,TRUE) ON DUPLICATE KEY UPDATE tenant_key = tenant_key", tenantKey)
	if err != nil {
		return Scope{}, fmt.Errorf("ensure tenant: %w", err)
	}
	var scope Scope
	err = tx.QueryRowContext(ctx, "SELECT id FROM tenants WHERE tenant_key = ?", tenantKey).Scan(&scope.TenantID)
	if err != nil {
		return Scope{}, fmt.Errorf("read tenant: %w", err)
	}
	result, err := tx.ExecContext(ctx, "INSERT INTO applications (tenant_id, app_key, admin_digest) VALUES (?, ?, ?)",
		scope.TenantID, appKey, digest[:])
	if err != nil {
		return Scope{}, fmt.Errorf("create application: %w", conflict(err))
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Scope{}, err
	}
	scope.AppID = uint64(id)
	return scope, tx.Commit()
}

func (s *Store) Admin(ctx context.Context, digest [32]byte) (Scope, error) {
	var scope Scope
	err := s.db.QueryRowContext(ctx, "SELECT tenant_id, id FROM applications WHERE admin_digest = ?", digest[:]).
		Scan(&scope.TenantID, &scope.AppID)
	if errors.Is(err, sql.ErrNoRows) {
		return Scope{}, ErrUnauthorized
	}
	return scope, err
}

func (s *Store) CreateUser(ctx context.Context, scope Scope, username, displayName string, hash []byte) (Account, error) {
	result, err := s.db.ExecContext(ctx, "INSERT INTO users (tenant_id, app_id, username, display_name, password_hash) VALUES (?, ?, ?, ?, ?)",
		scope.TenantID, scope.AppID, username, displayName, hash)
	if err != nil {
		return Account{}, fmt.Errorf("create user: %w", conflict(err))
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Account{}, err
	}
	return Account{Scope: scope, UserID: uint64(id), Username: username, DisplayName: displayName}, nil
}

func (s *Store) Credentials(ctx context.Context, tenantKey, appKey, username string) (Account, []byte, error) {
	const query = `
		SELECT u.tenant_id, u.app_id, u.id, u.username, u.display_name, u.password_hash
		FROM tenants t
		JOIN applications a ON a.tenant_id = t.id
		JOIN users u ON u.tenant_id = a.tenant_id AND u.app_id = a.id
		WHERE t.tenant_key = ? AND a.app_key = ? AND u.username = ?`
	var account Account
	var hash []byte
	err := s.db.QueryRowContext(ctx, query, tenantKey, appKey, username).Scan(
		&account.TenantID, &account.AppID, &account.UserID, &account.Username, &account.DisplayName, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, nil, ErrUnauthorized
	}
	return account, hash, err
}

func (s *Store) LoginDevice(ctx context.Context, account Account, deviceKey string, digest [32]byte) (Session, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, err
	}
	defer tx.Rollback()
	var userID uint64
	err = tx.QueryRowContext(ctx, "SELECT id FROM users WHERE tenant_id = ? AND app_id = ? AND id = ? FOR UPDATE",
		account.TenantID, account.AppID, account.UserID).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrUnauthorized
	}
	if err != nil {
		return Session{}, err
	}
	const refresh = `
		UPDATE devices SET token_digest = ?, credential_version = credential_version + 1,
			expires_at = UTC_TIMESTAMP(6) + INTERVAL 30 DAY, revoked_at = NULL
		WHERE tenant_id = ? AND app_id = ? AND user_id = ? AND device_key = ?`
	result, err := tx.ExecContext(ctx, refresh, digest[:], account.TenantID, account.AppID, userID, deviceKey)
	if err != nil {
		return Session{}, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return Session{}, err
	}
	if count == 0 {
		const insert = `
			INSERT INTO devices (tenant_id, app_id, user_id, device_key, token_digest, expires_at)
			VALUES (?, ?, ?, ?, ?, UTC_TIMESTAMP(6) + INTERVAL 30 DAY)`
		if _, err := tx.ExecContext(ctx, insert, account.TenantID, account.AppID, userID, deviceKey, digest[:]); err != nil {
			return Session{}, err
		}
	}
	session, err := findSession(ctx, tx, digest)
	if err != nil {
		return Session{}, err
	}
	return session, tx.Commit()
}

func (s *Store) Session(ctx context.Context, digest [32]byte) (Session, error) {
	return findSession(ctx, s.db, digest)
}

type rowQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func findSession(ctx context.Context, db rowQueryer, digest [32]byte) (Session, error) {
	const query = `
		SELECT d.tenant_id, d.app_id, d.user_id, u.username, u.display_name,
			d.id, d.device_key, d.expires_at, d.credential_version
		FROM devices d JOIN users u ON u.tenant_id = d.tenant_id AND u.app_id = d.app_id AND u.id = d.user_id
		WHERE d.token_digest = ? AND d.revoked_at IS NULL AND d.expires_at > UTC_TIMESTAMP(6)`
	var session Session
	err := db.QueryRowContext(ctx, query, digest[:]).Scan(
		&session.TenantID, &session.AppID, &session.UserID, &session.Username, &session.DisplayName,
		&session.DeviceID, &session.DeviceKey, &session.ExpiresAt, &session.CredentialVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrUnauthorized
	}
	return session, err
}

func (s *Store) ListDevices(ctx context.Context, session Session, after uint64) ([]Device, error) {
	const query = `
		SELECT id, device_key, expires_at, revoked_at FROM devices
		WHERE tenant_id = ? AND app_id = ? AND user_id = ? AND id > ? ORDER BY id LIMIT 100`
	rows, err := s.db.QueryContext(ctx, query, session.TenantID, session.AppID, session.UserID, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	devices := make([]Device, 0)
	for rows.Next() {
		var device Device
		if err := rows.Scan(&device.DeviceID, &device.DeviceKey, &device.ExpiresAt, &device.RevokedAt); err != nil {
			return nil, err
		}
		devices = append(devices, device)
	}
	return devices, rows.Err()
}

func (s *Store) Rotate(ctx context.Context, session Session, digest [32]byte) (Session, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, err
	}
	defer tx.Rollback()
	const query = `
		UPDATE devices SET token_digest = ?, credential_version = credential_version + 1, expires_at = UTC_TIMESTAMP(6) + INTERVAL 30 DAY
		WHERE tenant_id = ? AND app_id = ? AND user_id = ? AND id = ? AND credential_version = ?
			AND revoked_at IS NULL AND expires_at > UTC_TIMESTAMP(6)`
	updated, err := tx.ExecContext(ctx, query, digest[:], session.TenantID, session.AppID, session.UserID, session.DeviceID, session.CredentialVersion)
	if err != nil {
		return Session{}, err
	}
	count, err := updated.RowsAffected()
	if err != nil {
		return Session{}, err
	}
	if count != 1 {
		return Session{}, ErrUnauthorized
	}
	next, err := findSession(ctx, tx, digest)
	if err != nil {
		return Session{}, err
	}
	return next, tx.Commit()
}

func (s *Store) Revoke(ctx context.Context, current Session, targetID uint64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Lock the user before the device rows, in the same order as LoginDevice.
	var userID uint64
	err = tx.QueryRowContext(ctx, "SELECT id FROM users WHERE tenant_id = ? AND app_id = ? AND id = ? FOR UPDATE",
		current.TenantID, current.AppID, current.UserID).Scan(&userID)
	if err != nil {
		return err
	}
	const active = `
		SELECT id FROM devices WHERE tenant_id = ? AND app_id = ? AND user_id = ? AND id = ?
			AND credential_version = ? AND revoked_at IS NULL AND expires_at > UTC_TIMESTAMP(6) FOR UPDATE`
	var activeID uint64
	err = tx.QueryRowContext(ctx, active, current.TenantID, current.AppID, userID, current.DeviceID, current.CredentialVersion).Scan(&activeID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrUnauthorized
	}
	if err != nil {
		return err
	}
	const target = "SELECT revoked_at FROM devices WHERE tenant_id = ? AND app_id = ? AND user_id = ? AND id = ? FOR UPDATE"
	var revokedAt sql.NullTime
	err = tx.QueryRowContext(ctx, target, current.TenantID, current.AppID, userID, targetID).Scan(&revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if !revokedAt.Valid {
		const revoke = `
			UPDATE devices SET token_digest = NULL, revoked_at = UTC_TIMESTAMP(6), credential_version = credential_version + 1
			WHERE tenant_id = ? AND app_id = ? AND user_id = ? AND id = ?`
		if _, err := tx.ExecContext(ctx, revoke, current.TenantID, current.AppID, userID, targetID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
