package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

type GrantChange struct {
	UserID          uint64    `json:"user_id"`
	ExpectedVersion uint64    `json:"expected_version"`
	Role            string    `json:"duty_role"`
	ExpiresAt       time.Time `json:"expires_at"`
	Revoke          bool      `json:"revoke"`
}

type GrantReceipt struct {
	ID          uint64                      `json:"grant_id"`
	UserID      uint64                      `json:"user_id"`
	LinkID      uint64                      `json:"link_id"`
	Epoch       uint64                      `json:"grant_epoch"`
	Version     uint64                      `json:"grant_version"`
	ActiveSeats int                         `json:"active_seats"`
	Role        string                      `json:"duty_role"`
	ExpiresAt   time.Time                   `json:"expires_at"`
	Revoked     bool                        `json:"revoked"`
	Devices     []SharedDeviceAuthorization `json:"device_authorizations,omitempty"`
	MoreDevices bool                        `json:"more_device_authorizations,omitempty"`
}

func (s *Store) ChangeSharedGrant(ctx context.Context, actor Scope, link uint64, key string, input GrantChange) (GrantReceipt, error) {
	var out GrantReceipt
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var host, peer Scope
	var state string
	var limit int
	var epoch, lastProjectSeq uint64
	var now, expires time.Time
	err = tx.QueryRowContext(ctx, `SELECT s.host_tenant,s.host_app,l.peer_tenant,l.peer_app,l.state,l.grant_epoch,l.last_project_seq,i.seat_limit,i.project_ends_at,UTC_TIMESTAMP(6)
		FROM partner_links l JOIN shared_spaces s ON s.id=l.space_id JOIN partner_invitations i ON i.id=l.invitation_id
		WHERE l.id=? AND ((l.peer_tenant=? AND l.peer_app=?) OR (s.host_tenant=? AND s.host_app=?)) FOR UPDATE OF l`,
		link, actor.TenantID, actor.AppID, actor.TenantID, actor.AppID).Scan(&host.TenantID, &host.AppID, &peer.TenantID, &peer.AppID, &state, &epoch, &lastProjectSeq, &limit, &expires, &now)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return out, err
	}
	digest := sha256.Sum256(raw)
	var priorHash, priorJSON []byte
	err = tx.QueryRowContext(ctx, "SELECT request_digest,receipt FROM shared_grant_changes WHERE link_id=? AND actor_app=? AND request_key=?", link, actor.AppID, key).Scan(&priorHash, &priorJSON)
	if err == nil {
		if !bytes.Equal(priorHash, digest[:]) {
			return out, ErrPayloadConflict
		}
		if err = json.Unmarshal(priorJSON, &out); err != nil {
			return out, err
		}
		return out, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	if state == "closed" || (!input.Revoke && (state != "active" || !input.ExpiresAt.After(now) || input.ExpiresAt.After(expires))) {
		return out, ErrConflict
	}
	var target Scope
	err = tx.QueryRowContext(ctx, "SELECT tenant_id,app_id FROM users WHERE id=? AND app_id IN (?,?)", input.UserID, host.AppID, peer.AppID).Scan(&target.TenantID, &target.AppID)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	if !input.Revoke && target != actor {
		return out, ErrForbidden
	}
	out = GrantReceipt{UserID: input.UserID, LinkID: link, Epoch: epoch + 1, Role: input.Role, ExpiresAt: input.ExpiresAt, Revoked: input.Revoke}
	var oldExpiry time.Time
	var revoked sql.NullTime
	var oldRole string
	err = tx.QueryRowContext(ctx, "SELECT id,grant_version,duty_role,expires_at,revoked_at FROM shared_grants WHERE link_id=? AND app_id=? AND user_id=? FOR UPDATE",
		link, target.AppID, input.UserID).Scan(&out.ID, &out.Version, &oldRole, &oldExpiry, &revoked)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	if input.ExpectedVersion != out.Version {
		return out, ErrConflict
	}
	active := out.ID != 0 && !revoked.Valid && oldExpiry.After(now)
	if input.Revoke && (out.ID == 0 || revoked.Valid) {
		return out, ErrConflict
	}
	err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM shared_grants WHERE link_id=? AND app_id=? AND revoked_at IS NULL AND expires_at>?", link, target.AppID, now).Scan(&out.ActiveSeats)
	if err != nil {
		return out, err
	}
	out.Version++
	switch {
	case input.Revoke:
		out.ExpiresAt, out.Role = oldExpiry, oldRole
		if active {
			out.ActiveSeats--
		}
		_, err = tx.ExecContext(ctx, "UPDATE shared_grants SET grant_version=?,revoked_at=? WHERE id=?", out.Version, now, out.ID)
	case !active && target == peer && out.ActiveSeats >= limit:
		return out, ErrConflict
	case out.ID == 0:
		var result sql.Result
		result, err = tx.ExecContext(ctx, `INSERT INTO shared_grants (link_id,tenant_id,app_id,user_id,grant_version,duty_role,joined_at,expires_at,first_project_seq) VALUES (?,?,?,?,?,?,?,?,?)`,
			link, target.TenantID, target.AppID, input.UserID, out.Version, out.Role, now, out.ExpiresAt, lastProjectSeq+1)
		if err == nil {
			var id int64
			id, err = result.LastInsertId()
			out.ID = uint64(id)
		}
		out.ActiveSeats++
	default:
		_, err = tx.ExecContext(ctx, `UPDATE shared_grants SET grant_version=?,duty_role=?,joined_at=IF(?,joined_at,?),first_project_seq=IF(?,first_project_seq,?),expires_at=?,revoked_at=NULL WHERE id=?`,
			out.Version, out.Role, active, now, active, lastProjectSeq+1, out.ExpiresAt, out.ID)
		if !active {
			out.ActiveSeats++
		}
	}
	if err != nil {
		return out, err
	}
	if input.Revoke {
		out.Devices, out.MoreDevices, err = sharedDeviceAuthorizations(ctx, tx, link, target, input.UserID)
		if err != nil {
			return out, err
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE partner_links SET grant_epoch=? WHERE id=?", out.Epoch, link); err != nil {
		return out, err
	}
	raw, err = json.Marshal(out)
	if err != nil {
		return out, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO shared_grant_changes (link_id,actor_tenant,actor_app,request_key,request_digest,grant_id,receipt) VALUES (?,?,?,?,?,?,?)`,
		link, actor.TenantID, actor.AppID, key, digest[:], out.ID, raw); err != nil {
		return out, err
	}
	action := "grant_set"
	if input.Revoke {
		action = "grant_revoked"
	}
	detail := struct {
		RequestKey string `json:"request_key"`
		GrantReceipt
	}{RequestKey: key, GrantReceipt: out}
	if err = recordSharedAudit(ctx, tx, link, actor, action, out.Epoch, detail); err != nil {
		return out, err
	}
	return out, tx.Commit()
}
