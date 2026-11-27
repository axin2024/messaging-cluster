package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

const sharedExportPageSize = 200
const sharedExportAttempts = 5

type exportCandidate struct {
	id, link uint64
	actor    Scope
	attempts int
}

type exportedSharedMessage struct {
	ChannelID uint64          `json:"channel_id"`
	Channel   string          `json:"channel_key"`
	Audience  string          `json:"audience"`
	Order     uint64          `json:"project_seq"`
	Seq       uint64          `json:"seq"`
	Sender    uint64          `json:"sender_user"`
	Body      string          `json:"body"`
	Root      *uint64         `json:"root_seq,omitempty"`
	Created   time.Time       `json:"created_at"`
	Files     json.RawMessage `json:"attachments"`
	deadline  sql.NullTime
}

func (s *Store) ProcessSharedExports(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id,link_id,actor_tenant,actor_app,attempts FROM shared_exports
		WHERE state='queued' AND next_attempt_at<=UTC_TIMESTAMP(6) ORDER BY next_attempt_at,id LIMIT 8`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var pending []exportCandidate
	for rows.Next() {
		var job exportCandidate
		if err = rows.Scan(&job.id, &job.link, &job.actor.TenantID, &job.actor.AppID, &job.attempts); err != nil {
			return err
		}
		pending = append(pending, job)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	var failures error
	for _, job := range pending {
		if err = s.processSharedExport(ctx, job); err == nil {
			continue
		}
		if ctx.Err() != nil {
			return errors.Join(failures, err)
		}
		next := job.attempts + 1
		_, updateErr := s.db.ExecContext(ctx, `UPDATE shared_exports SET attempts=?,last_error='snapshot_processing_failed',
			state=IF(?,'failed','queued'),finished_at=IF(?,UTC_TIMESTAMP(6),NULL),
			next_attempt_at=TIMESTAMPADD(SECOND,?,UTC_TIMESTAMP(6)) WHERE id=? AND state='queued' AND attempts=?`,
			next, next >= sharedExportAttempts, next >= sharedExportAttempts, 1<<next, job.id, job.attempts)
		failures = errors.Join(failures, err, updateErr)
	}
	return failures
}

func (s *Store) processSharedExport(ctx context.Context, job exportCandidate) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	access, err := lockSharedAdminLink(ctx, tx, job.actor, job.link, false)
	if err != nil {
		return err
	}
	out, err := readSharedExport(tx.QueryRowContext(ctx, `SELECT `+sharedExportColumns+` FROM shared_exports
		WHERE id=? AND state='queued' AND next_attempt_at<=UTC_TIMESTAMP(6) FOR UPDATE SKIP LOCKED`, job.id))
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if currentExport(out, access).State != "queued" {
		_, err = tx.ExecContext(ctx, `UPDATE shared_exports SET state='invalidated',finished_at=UTC_TIMESTAMP(6) WHERE id=?`, out.ID)
		if err != nil {
			return err
		}
		return tx.Commit()
	}
	items, err := exportMessagePage(ctx, tx, job.actor, out)
	if err != nil {
		return err
	}
	out.Through = out.Snapshot
	out.HasMore = len(items) > sharedExportPageSize
	if out.HasMore {
		items = items[:sharedExportPageSize]
		out.Through = items[len(items)-1].Order
	}
	archive, err := json.Marshal(struct {
		Format   string                  `json:"format"`
		Link     uint64                  `json:"link_id"`
		Actor    Scope                   `json:"organization"`
		After    uint64                  `json:"after_project_seq"`
		Through  uint64                  `json:"through_project_seq"`
		Snapshot uint64                  `json:"snapshot_project_seq"`
		More     bool                    `json:"has_more"`
		Items    []exportedSharedMessage `json:"messages"`
	}{"messaging-project-archive.v1", out.LinkID, job.actor, out.After, out.Through, out.Snapshot, out.HasMore, items})
	if err != nil {
		return err
	}
	digest := sha256.Sum256(archive)
	expires := out.ExpiresAt
	for _, item := range items {
		if item.deadline.Valid && item.deadline.Time.Before(expires) {
			expires = item.deadline.Time
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE shared_exports SET state='ready',attempts=attempts+1,last_error=NULL,archive_digest=?,archive=?,
		through_seq=?,item_count=?,has_more=?,expires_at=?,finished_at=UTC_TIMESTAMP(6) WHERE id=?`, digest[:], archive, out.Through, len(items), out.HasMore, expires, out.ID)
	if err != nil {
		return err
	}
	if err = recordSharedAudit(ctx, tx, job.link, job.actor, "export_ready", out.Epoch,
		map[string]any{"export_id": out.ID, "archive_digest": hex.EncodeToString(digest[:]), "item_count": len(items)}); err != nil {
		return err
	}
	return tx.Commit()
}

func exportMessagePage(ctx context.Context, tx *sql.Tx, actor Scope, out SharedExport) ([]exportedSharedMessage, error) {
	rows, err := tx.QueryContext(ctx, `SELECT m.channel_id,c.channel_key,c.audience,m.project_seq,m.seq,m.sender_user,m.body,m.root_seq,m.created_at,
		COALESCE((SELECT JSON_ARRAYAGG(JSON_OBJECT('id',a.id,'filename',a.filename,'size',a.byte_size,'sha256',LOWER(HEX(a.content_digest))))
		FROM shared_message_files f JOIN attachments a ON a.id=f.attachment_id AND a.tenant_id=f.tenant_id AND a.app_id=f.app_id
		WHERE f.channel_id=m.channel_id AND f.seq=m.seq),JSON_ARRAY()),m.retained_until`+exportableMessages+visibleSharedMessage("1")+
		` ORDER BY m.project_seq LIMIT ?`, out.LinkID, actor.TenantID, actor.AppID, out.After, out.Snapshot, sharedExportPageSize+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]exportedSharedMessage, 0, sharedExportPageSize)
	for rows.Next() {
		var m exportedSharedMessage
		var files []byte
		if err = rows.Scan(&m.ChannelID, &m.Channel, &m.Audience, &m.Order, &m.Seq, &m.Sender,
			&m.Body, &m.Root, &m.Created, &files, &m.deadline); err != nil {
			return nil, err
		}
		m.Files = files
		items = append(items, m)
	}
	return items, rows.Err()
}

func (s *Store) PruneSharedExports(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM shared_export_grants WHERE expires_at<=UTC_TIMESTAMP(6) ORDER BY expires_at LIMIT 2000`)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE shared_exports SET archive=NULL WHERE expires_at<=UTC_TIMESTAMP(6) AND archive IS NOT NULL LIMIT 200`)
	return err
}
