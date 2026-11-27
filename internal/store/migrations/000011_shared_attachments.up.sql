CREATE TABLE shared_message_files (
    channel_id BIGINT UNSIGNED NOT NULL,
    seq BIGINT UNSIGNED NOT NULL,
    ordinal TINYINT UNSIGNED NOT NULL,
    tenant_id BIGINT UNSIGNED NOT NULL,
    app_id BIGINT UNSIGNED NOT NULL,
    attachment_id BIGINT UNSIGNED NOT NULL,
    PRIMARY KEY (channel_id,seq,ordinal),
    UNIQUE KEY shared_file_reference (channel_id,seq,attachment_id),
    FOREIGN KEY (channel_id,seq) REFERENCES shared_messages(channel_id,seq),
    FOREIGN KEY (tenant_id,app_id,attachment_id) REFERENCES attachments(tenant_id,app_id,id)
) ENGINE=InnoDB;

CREATE TABLE shared_attachment_grants (
    token_digest BINARY(32) PRIMARY KEY,
    channel_id BIGINT UNSIGNED NOT NULL,
    seq BIGINT UNSIGNED NOT NULL,
    attachment_id BIGINT UNSIGNED NOT NULL,
    device_id BIGINT UNSIGNED NOT NULL,
    credential_version BIGINT UNSIGNED NOT NULL,
    grant_epoch BIGINT UNSIGNED NOT NULL,
    grant_version BIGINT UNSIGNED NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    KEY expire_tickets (expires_at),
    FOREIGN KEY (channel_id,seq,attachment_id) REFERENCES shared_message_files(channel_id,seq,attachment_id),
    FOREIGN KEY (device_id) REFERENCES devices(id)
) ENGINE=InnoDB;

CREATE TABLE shared_attachment_access (
    id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
    link_id BIGINT UNSIGNED NOT NULL,
    channel_id BIGINT UNSIGNED NOT NULL,
    seq BIGINT UNSIGNED NOT NULL,
    attachment_id BIGINT UNSIGNED NOT NULL,
    recipient_tenant BIGINT UNSIGNED NOT NULL,
    recipient_app BIGINT UNSIGNED NOT NULL,
    recipient_user BIGINT UNSIGNED NOT NULL,
    device_id BIGINT UNSIGNED NOT NULL,
    grant_epoch BIGINT UNSIGNED NOT NULL,
    authorized_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    KEY organization_receipts (link_id,recipient_app,id),
    FOREIGN KEY (link_id) REFERENCES partner_links(id),
    FOREIGN KEY (channel_id,seq,attachment_id) REFERENCES shared_message_files(channel_id,seq,attachment_id),
    FOREIGN KEY (recipient_tenant,recipient_app,recipient_user) REFERENCES users(tenant_id,app_id,id),
    FOREIGN KEY (device_id) REFERENCES devices(id)
) ENGINE=InnoDB;
