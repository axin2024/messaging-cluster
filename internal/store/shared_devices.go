package store

import (
	"context"
	"database/sql"
	"time"
)

type SharedDeviceAuthorization struct {
	DeviceID          uint64    `json:"device_id"`
	CredentialVersion uint64    `json:"credential_version"`
	GrantEpoch        uint64    `json:"grant_epoch"`
	GrantVersion      uint64    `json:"grant_version"`
	AuthorizedAt      time.Time `json:"authorized_at"`
}

func recordSharedDevice(ctx context.Context, tx *sql.Tx, session Session, a sharedAccess) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO shared_read_devices (link_id,tenant_id,app_id,user_id,device_id,credential_version,grant_epoch,grant_version)
		VALUES (?,?,?,?,?,?,?,?) AS seen ON DUPLICATE KEY UPDATE credential_version=seen.credential_version,
		grant_epoch=seen.grant_epoch,grant_version=seen.grant_version,authorized_at=UTC_TIMESTAMP(6)`,
		a.LinkID, session.TenantID, session.AppID, session.UserID, session.DeviceID, session.CredentialVersion, a.epoch, a.version)
	return err
}

func sharedDeviceAuthorizations(ctx context.Context, tx *sql.Tx, link uint64, target Scope, user uint64) ([]SharedDeviceAuthorization, bool, error) {
	const pageSize = 100
	rows, err := tx.QueryContext(ctx, `SELECT device_id,credential_version,grant_epoch,grant_version,authorized_at FROM shared_read_devices
		WHERE link_id=? AND tenant_id=? AND app_id=? AND user_id=? ORDER BY authorized_at DESC,device_id DESC LIMIT ?`,
		link, target.TenantID, target.AppID, user, pageSize+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	items := []SharedDeviceAuthorization{}
	for rows.Next() {
		if len(items) == pageSize {
			return items, true, nil
		}
		var item SharedDeviceAuthorization
		if err = rows.Scan(&item.DeviceID, &item.CredentialVersion, &item.GrantEpoch, &item.GrantVersion, &item.AuthorizedAt); err != nil {
			return nil, false, err
		}
		items = append(items, item)
	}
	return items, false, rows.Err()
}
