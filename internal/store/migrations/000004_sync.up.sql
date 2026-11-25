ALTER TABLE conversations ADD retained_from_seq BIGINT UNSIGNED NOT NULL DEFAULT 1,
    ADD CONSTRAINT conversation_retention CHECK (retained_from_seq >= 1 AND retained_from_seq <= last_seq + 1);

ALTER TABLE messages ADD expired_at DATETIME(6) NULL;

CREATE TABLE inbox (
    tenant_id BIGINT UNSIGNED NOT NULL,
    app_id BIGINT UNSIGNED NOT NULL,
    user_id BIGINT UNSIGNED NOT NULL,
    conversation_id BIGINT UNSIGNED NOT NULL,
    high_seq BIGINT UNSIGNED NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant_id, app_id, user_id, conversation_id),
    KEY inbox_conversation (tenant_id, app_id, conversation_id),
    CONSTRAINT inbox_user FOREIGN KEY (tenant_id, app_id, user_id) REFERENCES users (tenant_id, app_id, id),
    CONSTRAINT inbox_conversation FOREIGN KEY (tenant_id, app_id, conversation_id) REFERENCES conversations (tenant_id, app_id, id)
) ENGINE=InnoDB;

INSERT INTO inbox (tenant_id, app_id, user_id, conversation_id, high_seq)
SELECT m.tenant_id, m.app_id, m.user_id, m.conversation_id,
    MAX(CASE WHEN m.from_seq <= COALESCE(m.to_seq, c.last_seq) THEN COALESCE(m.to_seq, c.last_seq) ELSE 0 END)
FROM memberships AS m JOIN conversations AS c ON c.id = m.conversation_id
GROUP BY m.tenant_id, m.app_id, m.user_id, m.conversation_id;

CREATE TABLE sync_cursors (
    tenant_id BIGINT UNSIGNED NOT NULL,
    app_id BIGINT UNSIGNED NOT NULL,
    device_id BIGINT UNSIGNED NOT NULL,
    conversation_id BIGINT UNSIGNED NOT NULL,
    seq BIGINT UNSIGNED NOT NULL,
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (device_id, conversation_id),
    KEY sync_device (tenant_id, app_id, device_id),
    KEY sync_conversation (tenant_id, app_id, conversation_id),
    CONSTRAINT sync_device FOREIGN KEY (tenant_id, app_id, device_id) REFERENCES devices (tenant_id, app_id, id),
    CONSTRAINT sync_conversation FOREIGN KEY (tenant_id, app_id, conversation_id) REFERENCES conversations (tenant_id, app_id, id)
) ENGINE=InnoDB;

CREATE TABLE read_state (
    tenant_id BIGINT UNSIGNED NOT NULL,
    app_id BIGINT UNSIGNED NOT NULL,
    user_id BIGINT UNSIGNED NOT NULL,
    conversation_id BIGINT UNSIGNED NOT NULL,
    seq BIGINT UNSIGNED NOT NULL,
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (tenant_id, app_id, user_id, conversation_id),
    CONSTRAINT read_inbox FOREIGN KEY (tenant_id, app_id, user_id, conversation_id)
        REFERENCES inbox (tenant_id, app_id, user_id, conversation_id)
) ENGINE=InnoDB;

CREATE TABLE retention_runs (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id BIGINT UNSIGNED NOT NULL,
    app_id BIGINT UNSIGNED NOT NULL,
    conversation_id BIGINT UNSIGNED NOT NULL,
    requested_before DATETIME(6) NOT NULL,
    from_seq BIGINT UNSIGNED NOT NULL,
    to_seq BIGINT UNSIGNED NOT NULL,
    applied_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    KEY retention_conversation (tenant_id, app_id, conversation_id, id),
    CONSTRAINT retention_conversation FOREIGN KEY (tenant_id, app_id, conversation_id) REFERENCES conversations (tenant_id, app_id, id),
    CONSTRAINT retention_range CHECK (from_seq > 0 AND to_seq >= from_seq)
) ENGINE=InnoDB;
