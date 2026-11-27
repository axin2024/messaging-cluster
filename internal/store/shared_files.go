package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

func addSharedFiles(ctx context.Context, tx *sql.Tx, session Session, channel, seq uint64, ids []uint64) error {
	for index, id := range ids {
		result, err := tx.ExecContext(ctx, `INSERT INTO shared_message_files (channel_id,seq,ordinal,tenant_id,app_id,attachment_id)
			SELECT ?,?,?,tenant_id,app_id,id FROM attachments WHERE tenant_id=? AND app_id=? AND owner_id=? AND id=? AND ready_at IS NOT NULL`,
			channel, seq, index, session.TenantID, session.AppID, session.UserID, id)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return ErrInvalidAttachment
		}
	}
	return nil
}

func loadSharedFiles(ctx context.Context, tx *sql.Tx, channel uint64, items []SharedMessage) error {
	if len(items) == 0 {
		return nil
	}
	args := make([]any, 1, len(items)+1)
	args[0] = channel
	positions := make(map[uint64]int, len(items))
	for i, item := range items {
		args = append(args, item.Seq)
		positions[item.Seq] = i
	}
	rows, err := tx.QueryContext(ctx, `SELECT f.seq,a.id,a.filename,a.content_type,a.byte_size,a.content_digest,a.created_at,a.ready_at
		FROM shared_message_files f JOIN attachments a ON a.id=f.attachment_id AND a.tenant_id=f.tenant_id AND a.app_id=f.app_id
		WHERE f.channel_id=? AND f.seq IN (?`+strings.Repeat(",?", len(items)-1)+`) ORDER BY f.seq,f.ordinal`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var seq uint64
		var a Attachment
		var digest []byte
		if err = rows.Scan(&seq, &a.ID, &a.Filename, &a.ContentType, &a.Size, &digest, &a.CreatedAt, &a.ReadyAt); err != nil {
			return err
		}
		a.SHA256 = hex.EncodeToString(digest)
		at := positions[seq]
		items[at].Attachments = append(items[at].Attachments, a)
	}
	return rows.Err()
}

func sharedFile(ctx context.Context, tx *sql.Tx, access sharedAccess, seq, file uint64) (Attachment, uint64, error) {
	if err := checkSharedReference(ctx, tx, access, seq, false); err != nil {
		return Attachment{}, 0, err
	}
	object, err := scanAttachment(tx.QueryRowContext(ctx, `SELECT `+attachmentColumns+` FROM shared_message_files f
		JOIN attachments a ON a.id=f.attachment_id AND a.tenant_id=f.tenant_id AND a.app_id=f.app_id
		WHERE f.channel_id=? AND f.seq=? AND f.attachment_id=? AND a.ready_at IS NOT NULL`, access.ID, seq, file))
	if err != nil {
		return Attachment{}, 0, err
	}
	var sender uint64
	err = tx.QueryRowContext(ctx, `SELECT sender_tenant FROM shared_messages WHERE channel_id=? AND seq=?`, access.ID, seq).Scan(&sender)
	return object, sender, err
}

func (s *Store) CreateSharedFileGrant(ctx context.Context, session Session, channel, seq, file uint64, digest [32]byte) error {
	tx, err := s.beginConversationWrite(ctx, session)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	a, err := lockSharedAccess(ctx, tx, session, channel, false)
	if err != nil {
		return err
	}
	if _, _, err = sharedFile(ctx, tx, a, seq, file); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO shared_attachment_grants (token_digest,channel_id,seq,attachment_id,device_id,
		credential_version,grant_epoch,grant_version,expires_at) VALUES (?,?,?,?,?,?,?,?,TIMESTAMPADD(SECOND,?,UTC_TIMESTAMP(6)))`,
		digest[:], channel, seq, file, session.DeviceID, session.CredentialVersion, a.epoch, a.version, int64(AttachmentGrantTTL/time.Second))
	if err != nil {
		return err
	}
	if err = recordSharedDevice(ctx, tx, session, a); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RedeemSharedFileGrant(ctx context.Context, session Session, digest [32]byte) (Attachment, error) {
	tx, err := s.beginConversationWrite(ctx, session)
	if err != nil {
		return Attachment{}, err
	}
	defer tx.Rollback()
	var channel, seq, file, epoch, version uint64
	var expiry time.Time
	err = tx.QueryRowContext(ctx, `SELECT channel_id,seq,attachment_id,grant_epoch,grant_version,expires_at
		FROM shared_attachment_grants WHERE token_digest=? AND device_id=? AND credential_version=? AND expires_at>UTC_TIMESTAMP(6)`,
		digest[:], session.DeviceID, session.CredentialVersion).Scan(&channel, &seq, &file, &epoch, &version, &expiry)
	if errors.Is(err, sql.ErrNoRows) {
		return Attachment{}, ErrNotFound
	}
	if err != nil {
		return Attachment{}, err
	}
	access, err := lockSharedAccess(ctx, tx, session, channel, false)
	if err != nil {
		return Attachment{}, err
	}
	if epoch != access.epoch || version != access.version {
		return Attachment{}, ErrNotFound
	}
	var live bool
	if err = tx.QueryRowContext(ctx, `SELECT ?>UTC_TIMESTAMP(6)`, expiry).Scan(&live); err != nil {
		return Attachment{}, err
	}
	if !live {
		return Attachment{}, ErrNotFound
	}
	object, sender, err := sharedFile(ctx, tx, access, seq, file)
	if err != nil {
		return Attachment{}, err
	}
	if sender != session.TenantID {
		_, err = tx.ExecContext(ctx, `INSERT INTO shared_attachment_access (link_id,channel_id,seq,attachment_id,recipient_tenant,
			recipient_app,recipient_user,device_id,grant_epoch) VALUES (?,?,?,?,?,?,?,?,?)`, access.LinkID, channel, seq, file,
			session.TenantID, session.AppID, session.UserID, session.DeviceID, access.epoch)
		if err != nil {
			return Attachment{}, err
		}
		detail := map[string]uint64{"channel_id": channel, "seq": seq, "attachment_id": file,
			"recipient_user_id": session.UserID, "device_id": session.DeviceID}
		if err = recordSharedAudit(ctx, tx, access.LinkID, session.Scope, "attachment_download_authorized", access.epoch, detail); err != nil {
			return Attachment{}, err
		}
	}
	if err = recordSharedDevice(ctx, tx, session, access); err != nil {
		return Attachment{}, err
	}
	return object, tx.Commit()
}
