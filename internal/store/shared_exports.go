package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

const SharedExportPassTTL = 5 * time.Minute

type SharedExportInput struct {
	After    uint64  `json:"after_project_seq"`
	Snapshot *uint64 `json:"snapshot_project_seq,omitempty"`
}

type SharedExportPreview struct {
	Snapshot uint64 `json:"snapshot_project_seq"`
	Items    uint64 `json:"item_count"`
}

const exportableMessages = ` FROM shared_channels c JOIN shared_messages m ON m.channel_id=c.id
	WHERE c.link_id=? AND m.sender_tenant=? AND m.sender_app=? AND m.project_seq>? AND m.project_seq<=? AND `

func (s *Store) PreviewSharedExport(ctx context.Context, actor Scope, link uint64) (SharedExportPreview, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return SharedExportPreview{}, err
	}
	defer tx.Rollback()
	access, err := lockSharedAdminLink(ctx, tx, actor, link, false)
	if err != nil {
		return SharedExportPreview{}, err
	}
	if access.state != "active" || !access.live {
		return SharedExportPreview{}, ErrConflict
	}
	out := SharedExportPreview{Snapshot: access.projectSeq}
	err = tx.QueryRowContext(ctx, "SELECT COUNT(*)"+exportableMessages+visibleSharedMessage("1"),
		link, actor.TenantID, actor.AppID, 0, out.Snapshot).Scan(&out.Items)
	if err != nil {
		return SharedExportPreview{}, err
	}
	return out, tx.Commit()
}

type SharedExport struct {
	ID           uint64     `json:"export_id"`
	LinkID       uint64     `json:"link_id"`
	Epoch        uint64     `json:"grant_epoch"`
	After        uint64     `json:"after_project_seq"`
	Snapshot     uint64     `json:"snapshot_project_seq"`
	Through      uint64     `json:"through_project_seq"`
	Items        int        `json:"item_count"`
	HasMore      bool       `json:"has_more"`
	State        string     `json:"state"`
	Attempts     int        `json:"attempts"`
	LastError    *string    `json:"last_error,omitempty"`
	SHA256       string     `json:"archive_digest,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	ExpiresAt    time.Time  `json:"expires_at"`
	contentEpoch uint64
	requestHash  []byte
	live         bool
}

const sharedExportColumns = `id,link_id,grant_epoch,after_seq,snapshot_seq,through_seq,item_count,has_more,state,attempts,last_error,
	archive_digest,created_at,finished_at,expires_at,content_epoch,request_digest,expires_at>UTC_TIMESTAMP(6)`

func readSharedExport(row interface{ Scan(...any) error }) (SharedExport, error) {
	var out SharedExport
	var digest []byte
	err := row.Scan(&out.ID, &out.LinkID, &out.Epoch, &out.After, &out.Snapshot, &out.Through, &out.Items, &out.HasMore, &out.State,
		&out.Attempts, &out.LastError, &digest, &out.CreatedAt, &out.FinishedAt, &out.ExpiresAt, &out.contentEpoch, &out.requestHash, &out.live)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	out.SHA256 = hex.EncodeToString(digest)
	return out, err
}

func currentExport(out SharedExport, link sharedAdminLink) SharedExport {
	switch {
	case !out.live && link.state != "closed":
		out.State = "expired"
	case link.state != "active" || out.Epoch != link.epoch || out.contentEpoch != link.contentEpoch:
		out.State = "invalidated"
	}
	return out
}

func adminExport(ctx context.Context, tx *sql.Tx, actor Scope, link, id uint64) (SharedExport, error) {
	return readSharedExport(tx.QueryRowContext(ctx, `SELECT `+sharedExportColumns+` FROM shared_exports
		WHERE id=? AND link_id=? AND actor_tenant=? AND actor_app=?`, id, link, actor.TenantID, actor.AppID))
}

func (s *Store) CreateSharedExport(ctx context.Context, actor Scope, link uint64, key string, input SharedExportInput) (SharedExport, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return SharedExport{}, err
	}
	defer tx.Rollback()
	access, err := lockSharedAdminLink(ctx, tx, actor, link, true)
	if err != nil {
		return SharedExport{}, err
	}
	data, err := json.Marshal(input)
	if err != nil {
		return SharedExport{}, err
	}
	digest := sha256.Sum256(data)
	prior, err := readSharedExport(tx.QueryRowContext(ctx, `SELECT `+sharedExportColumns+` FROM shared_exports
		WHERE link_id=? AND actor_tenant=? AND actor_app=? AND request_key=?`, link, actor.TenantID, actor.AppID, key))
	if err == nil {
		if !bytes.Equal(prior.requestHash, digest[:]) {
			return SharedExport{}, ErrPayloadConflict
		}
		return currentExport(prior, access), tx.Commit()
	}
	if !errors.Is(err, ErrNotFound) {
		return SharedExport{}, err
	}
	if access.state != "active" || !access.live {
		return SharedExport{}, ErrConflict
	}
	snapshot := access.projectSeq
	if input.Snapshot != nil {
		snapshot = *input.Snapshot
	}
	if snapshot > access.projectSeq || input.After > snapshot {
		return SharedExport{}, ErrExportRange
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO shared_exports (link_id,actor_tenant,actor_app,request_key,request_digest,grant_epoch,
		content_epoch,after_seq,snapshot_seq,expires_at) VALUES (?,?,?,?,?,?,?,?,?,LEAST(?,TIMESTAMPADD(HOUR,24,UTC_TIMESTAMP(6))))`,
		link, actor.TenantID, actor.AppID, key, digest[:], access.epoch, access.contentEpoch, input.After, snapshot, access.expiresAt)
	if err != nil {
		return SharedExport{}, err
	}
	newID, err := result.LastInsertId()
	if err != nil {
		return SharedExport{}, err
	}
	out, err := adminExport(ctx, tx, actor, link, uint64(newID))
	if err != nil {
		return SharedExport{}, err
	}
	detail := map[string]any{"export_id": out.ID, "request_key": key, "snapshot_project_seq": snapshot}
	if err = recordSharedAudit(ctx, tx, link, actor, "export_requested", access.epoch, detail); err != nil {
		return SharedExport{}, err
	}
	return out, tx.Commit()
}

func (s *Store) SharedExportStatus(ctx context.Context, actor Scope, link, id uint64) (SharedExport, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return SharedExport{}, err
	}
	defer tx.Rollback()
	access, err := lockSharedAdminLink(ctx, tx, actor, link, false)
	if err != nil {
		return SharedExport{}, err
	}
	out, err := adminExport(ctx, tx, actor, link, id)
	if err != nil {
		return SharedExport{}, err
	}
	return currentExport(out, access), tx.Commit()
}

func (s *Store) RetrySharedExport(ctx context.Context, actor Scope, link, id uint64) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	access, err := lockSharedAdminLink(ctx, tx, actor, link, true)
	if err != nil {
		return err
	}
	out, err := adminExport(ctx, tx, actor, link, id)
	if err != nil {
		return err
	}
	switch currentExport(out, access).State {
	case "queued":
		return tx.Commit()
	case "failed":
	case "invalidated", "expired":
		return ErrNotFound
	default:
		return ErrConflict
	}
	_, err = tx.ExecContext(ctx, `UPDATE shared_exports SET state='queued',attempts=0,last_error=NULL,finished_at=NULL,
		next_attempt_at=UTC_TIMESTAMP(6) WHERE id=?`, id)
	if err != nil {
		return err
	}
	if err = recordSharedAudit(ctx, tx, link, actor, "export_retried", access.epoch, map[string]uint64{"export_id": id}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CreateSharedExportPass(ctx context.Context, actor Scope, link, id uint64, token [32]byte) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	access, err := lockSharedAdminLink(ctx, tx, actor, link, false)
	if err != nil {
		return err
	}
	out, err := adminExport(ctx, tx, actor, link, id)
	if err != nil {
		return err
	}
	state := currentExport(out, access).State
	if state == "invalidated" || state == "expired" {
		return ErrNotFound
	}
	if state != "ready" {
		return ErrConflict
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO shared_export_grants (token_digest,export_id,expires_at)
		VALUES (?,?,LEAST(?,TIMESTAMPADD(SECOND,?,UTC_TIMESTAMP(6))))`, token[:], id, out.ExpiresAt, int(SharedExportPassTTL/time.Second))
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RedeemSharedExportPass(ctx context.Context, actor Scope, link, id uint64, token [32]byte) ([]byte, string, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback()
	access, err := lockSharedAdminLink(ctx, tx, actor, link, false)
	if err != nil {
		return nil, "", err
	}
	if access.state != "active" || !access.live {
		return nil, "", ErrNotFound
	}
	var archive, digest []byte
	err = tx.QueryRowContext(ctx, `SELECT e.archive,e.archive_digest FROM shared_exports e JOIN shared_export_grants p ON p.export_id=e.id
		WHERE p.token_digest=? AND p.expires_at>UTC_TIMESTAMP(6) AND e.expires_at>UTC_TIMESTAMP(6) AND e.state='ready'
		AND e.archive IS NOT NULL AND e.id=? AND e.link_id=? AND e.actor_tenant=? AND e.actor_app=? AND e.grant_epoch=? AND e.content_epoch=?`,
		token[:], id, link, actor.TenantID, actor.AppID, access.epoch, access.contentEpoch).Scan(&archive, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	sha := hex.EncodeToString(digest)
	if err = recordSharedAudit(ctx, tx, link, actor, "export_download_authorized", access.epoch,
		map[string]any{"export_id": id, "archive_digest": sha}); err != nil {
		return nil, "", err
	}
	return archive, sha, tx.Commit()
}
