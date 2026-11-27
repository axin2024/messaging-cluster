package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

const AttachmentGrantTTL = time.Minute

type Attachment struct {
	ID          uint64     `json:"id"`
	Filename    string     `json:"filename"`
	ContentType string     `json:"content_type"`
	Size        int64      `json:"size"`
	SHA256      string     `json:"sha256"`
	CreatedAt   time.Time  `json:"created_at"`
	ReadyAt     *time.Time `json:"ready_at,omitempty"`
	ObjectKey   *string    `json:"-"`
	Digest      [32]byte   `json:"-"`
}

const attachmentColumns = "a.id, a.filename, a.content_type, a.byte_size, a.content_digest, a.created_at, a.ready_at, a.object_key"

func scanAttachment(row interface{ Scan(...any) error }) (Attachment, error) {
	var attachment Attachment
	var digest []byte
	err := row.Scan(&attachment.ID, &attachment.Filename, &attachment.ContentType, &attachment.Size,
		&digest, &attachment.CreatedAt, &attachment.ReadyAt, &attachment.ObjectKey)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	if err == nil {
		attachment.SHA256 = hex.EncodeToString(digest)
		attachment.Digest = [32]byte(digest)
	}
	return attachment, err
}

func (s *Store) CreateAttachment(ctx context.Context, session Session, filename, contentType string, size int64, digest [32]byte) (Attachment, error) {
	tx, err := s.beginConversationWrite(ctx, session)
	if err != nil {
		return Attachment{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO attachments (tenant_id, app_id, owner_id, filename, content_type, byte_size, content_digest)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, session.TenantID, session.AppID, session.UserID, filename, contentType, size, digest[:])
	if err != nil {
		return Attachment{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Attachment{}, err
	}
	attachment, err := scanAttachment(tx.QueryRowContext(ctx, `SELECT `+attachmentColumns+` FROM attachments AS a WHERE a.id = ?`, id))
	if err != nil {
		return Attachment{}, err
	}
	return attachment, tx.Commit()
}

const uploadAttachmentQuery = `SELECT ` + attachmentColumns + ` FROM attachments AS a
	WHERE a.tenant_id = ? AND a.app_id = ? AND a.owner_id = ? AND a.id = ?`

func (s *Store) UploadAttachment(ctx context.Context, session Session, id uint64) (Attachment, error) {
	return scanAttachment(s.db.QueryRowContext(ctx, uploadAttachmentQuery, session.TenantID, session.AppID, session.UserID, id))
}

// CompleteAttachment returns stored=false when another attempt finalized first.
// An error may reflect an unknown commit result, so the caller retains the file.
func (s *Store) CompleteAttachment(ctx context.Context, session Session, id uint64, key string) (attachment Attachment, stored bool, err error) {
	tx, err := s.beginConversationWrite(ctx, session)
	if err != nil {
		return Attachment{}, false, err
	}
	defer tx.Rollback()
	current, err := scanAttachment(tx.QueryRowContext(ctx, uploadAttachmentQuery+` FOR UPDATE`, session.TenantID, session.AppID, session.UserID, id))
	if err != nil {
		return Attachment{}, false, err
	}
	if current.ReadyAt != nil {
		return current, false, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE attachments SET object_key = ?, ready_at = UTC_TIMESTAMP(6) WHERE id = ?`, key, id); err != nil {
		return Attachment{}, false, err
	}
	current, err = scanAttachment(tx.QueryRowContext(ctx, uploadAttachmentQuery, session.TenantID, session.AppID, session.UserID, id))
	if err != nil {
		return Attachment{}, false, err
	}
	return current, true, tx.Commit()
}

func addMessageAttachments(ctx context.Context, tx *sql.Tx, session Session, id, seq uint64, attachments []uint64) error {
	for index, attachment := range attachments {
		result, err := tx.ExecContext(ctx, `INSERT INTO message_attachments (tenant_id, app_id, conversation_id, seq, attachment_id, ordinal)
			SELECT tenant_id, app_id, ?, ?, id, ? FROM attachments
			WHERE tenant_id = ? AND app_id = ? AND owner_id = ? AND id = ? AND ready_at IS NOT NULL`,
			id, seq, index, session.TenantID, session.AppID, session.UserID, attachment)
		if err != nil {
			return err
		}
		added, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if added != 1 {
			return ErrInvalidAttachment
		}
	}
	return nil
}

// A grant is valid only while its device credential and current media rights are valid.
const grantAccess = `d.tenant_id = m.tenant_id AND d.app_id = m.app_id AND d.revoked_at IS NULL AND d.expires_at > UTC_TIMESTAMP(6)
	AND m.expired_at IS NULL AND m.retracted_at IS NULL AND a.ready_at IS NOT NULL
	AND EXISTS (SELECT 1 FROM memberships AS visible WHERE visible.conversation_id = m.conversation_id AND visible.user_id = d.user_id
		AND visible.from_seq <= m.seq AND (visible.to_seq IS NULL OR visible.to_seq >= m.seq))`

func (s *Store) CreateAttachmentGrant(ctx context.Context, session Session, id, seq, attachment uint64, digest [32]byte) error {
	result, err := s.db.ExecContext(ctx, `INSERT INTO attachment_grants (token_digest, conversation_id, seq, attachment_id, device_id, credential_version, membership_id, expires_at)
		SELECT ?, m.conversation_id, m.seq, a.id, d.id, d.credential_version, active.id, TIMESTAMPADD(SECOND, ?, UTC_TIMESTAMP(6))
		FROM messages AS m JOIN message_attachments AS ma ON ma.conversation_id = m.conversation_id AND ma.seq = m.seq
		JOIN attachments AS a ON a.id = ma.attachment_id AND a.tenant_id = m.tenant_id AND a.app_id = m.app_id
		JOIN devices AS d ON d.id = ? AND d.credential_version = ?
		JOIN memberships AS active ON active.conversation_id = m.conversation_id AND active.active_user_id = d.user_id
		WHERE m.conversation_id = ? AND m.seq = ? AND a.id = ? AND `+grantAccess,
		digest[:], int64(AttachmentGrantTTL/time.Second), session.DeviceID, session.CredentialVersion, id, seq, attachment)
	if err != nil {
		return err
	}
	added, err := result.RowsAffected()
	if err == nil && added != 1 {
		err = ErrNotFound
	}
	return err
}

func (s *Store) RedeemAttachmentGrant(ctx context.Context, digest [32]byte) (Attachment, error) {
	return scanAttachment(s.db.QueryRowContext(ctx, `SELECT `+attachmentColumns+` FROM attachment_grants AS g
		JOIN messages AS m ON m.conversation_id = g.conversation_id AND m.seq = g.seq
		JOIN attachments AS a ON a.id = g.attachment_id AND a.tenant_id = m.tenant_id AND a.app_id = m.app_id
		JOIN devices AS d ON d.id = g.device_id AND d.credential_version = g.credential_version
		JOIN memberships AS active ON active.id = g.membership_id AND active.conversation_id = m.conversation_id AND active.active_user_id = d.user_id
		WHERE g.token_digest = ? AND g.expires_at > UTC_TIMESTAMP(6) AND `+grantAccess, digest[:]))
}

func (s *Store) PruneAttachmentGrants(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM attachment_grants WHERE expires_at <= UTC_TIMESTAMP(6) ORDER BY expires_at LIMIT 2000`)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM shared_attachment_grants WHERE expires_at <= UTC_TIMESTAMP(6) ORDER BY expires_at LIMIT 2000`)
	return err
}
