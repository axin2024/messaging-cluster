ALTER TABLE messages ADD retracted_at DATETIME(6) NULL;

CREATE TABLE attachments (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id BIGINT UNSIGNED NOT NULL,
    app_id BIGINT UNSIGNED NOT NULL,
    owner_id BIGINT UNSIGNED NOT NULL,
    filename VARCHAR(180) NOT NULL,
    content_type VARCHAR(127) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    byte_size BIGINT UNSIGNED NOT NULL,
    content_digest BINARY(32) NOT NULL,
    object_key VARCHAR(40) CHARACTER SET ascii COLLATE ascii_bin NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    ready_at DATETIME(6) NULL,
    PRIMARY KEY (id),
    UNIQUE KEY app_attachment (tenant_id, app_id, id),
    UNIQUE KEY attachment_object (object_key),
    KEY attachment_owner (tenant_id, app_id, owner_id),
    CONSTRAINT attachments_owner FOREIGN KEY (tenant_id, app_id, owner_id) REFERENCES users (tenant_id, app_id, id),
    CONSTRAINT attachment_ready CHECK ((object_key IS NULL) = (ready_at IS NULL)),
    CONSTRAINT attachment_size CHECK (byte_size BETWEEN 1 AND 10485760)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE message_attachments (
    tenant_id BIGINT UNSIGNED NOT NULL,
    app_id BIGINT UNSIGNED NOT NULL,
    conversation_id BIGINT UNSIGNED NOT NULL,
    seq BIGINT UNSIGNED NOT NULL,
    attachment_id BIGINT UNSIGNED NOT NULL,
    ordinal TINYINT UNSIGNED NOT NULL,
    PRIMARY KEY (conversation_id, seq, ordinal),
    UNIQUE KEY message_attachment (conversation_id, seq, attachment_id),
    KEY attachment_reference (tenant_id, app_id, attachment_id),
    CONSTRAINT message_attachment_message FOREIGN KEY (conversation_id, seq) REFERENCES messages (conversation_id, seq),
    CONSTRAINT message_attachment_object FOREIGN KEY (tenant_id, app_id, attachment_id) REFERENCES attachments (tenant_id, app_id, id)
) ENGINE=InnoDB;

CREATE TABLE attachment_grants (
    token_digest BINARY(32) NOT NULL,
    conversation_id BIGINT UNSIGNED NOT NULL,
    seq BIGINT UNSIGNED NOT NULL,
    attachment_id BIGINT UNSIGNED NOT NULL,
    device_id BIGINT UNSIGNED NOT NULL,
    credential_version BIGINT UNSIGNED NOT NULL,
    membership_id BIGINT UNSIGNED NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    PRIMARY KEY (token_digest),
    KEY attachment_grant_expiry (expires_at),
    KEY attachment_grant_reference (conversation_id, seq, attachment_id),
    CONSTRAINT attachment_grant_reference FOREIGN KEY (conversation_id, seq, attachment_id)
        REFERENCES message_attachments (conversation_id, seq, attachment_id),
    CONSTRAINT attachment_grant_device FOREIGN KEY (device_id) REFERENCES devices (id),
    CONSTRAINT attachment_grant_membership FOREIGN KEY (membership_id) REFERENCES memberships (id)
) ENGINE=InnoDB;

CREATE TABLE message_retractions (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id BIGINT UNSIGNED NOT NULL,
    app_id BIGINT UNSIGNED NOT NULL,
    conversation_id BIGINT UNSIGNED NOT NULL,
    seq BIGINT UNSIGNED NOT NULL,
    actor_id BIGINT UNSIGNED NOT NULL,
    retracted_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY retraction_message (conversation_id, seq),
    KEY retraction_timeline (conversation_id, id),
    KEY retraction_actor (tenant_id, app_id, actor_id),
    CONSTRAINT retraction_message FOREIGN KEY (conversation_id, seq) REFERENCES messages (conversation_id, seq),
    CONSTRAINT retraction_actor FOREIGN KEY (tenant_id, app_id, actor_id) REFERENCES users (tenant_id, app_id, id)
) ENGINE=InnoDB;
