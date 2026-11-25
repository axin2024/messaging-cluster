CREATE TABLE conversations (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id BIGINT UNSIGNED NOT NULL,
    app_id BIGINT UNSIGNED NOT NULL,
    kind ENUM('direct', 'group') NOT NULL,
    title VARCHAR(100) NOT NULL DEFAULT '',
    history_policy ENUM('all', 'since_join') NOT NULL,
    direct_low BIGINT UNSIGNED NULL,
    direct_high BIGINT UNSIGNED NULL,
    last_seq BIGINT UNSIGNED NOT NULL DEFAULT 0,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY app_conversation (tenant_id, app_id, id),
    UNIQUE KEY direct_pair (tenant_id, app_id, direct_low, direct_high),
    CONSTRAINT conversation_app FOREIGN KEY (tenant_id, app_id) REFERENCES applications (tenant_id, id),
    CONSTRAINT conversation_direct_low FOREIGN KEY (tenant_id, app_id, direct_low) REFERENCES users (tenant_id, app_id, id),
    CONSTRAINT conversation_direct_high FOREIGN KEY (tenant_id, app_id, direct_high) REFERENCES users (tenant_id, app_id, id),
    CONSTRAINT conversation_kind CHECK (
        (kind = 'direct' AND direct_low IS NOT NULL AND direct_high IS NOT NULL AND direct_low < direct_high AND history_policy = 'all')
        OR (kind = 'group' AND direct_low IS NULL AND direct_high IS NULL)
    )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE memberships (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id BIGINT UNSIGNED NOT NULL,
    app_id BIGINT UNSIGNED NOT NULL,
    conversation_id BIGINT UNSIGNED NOT NULL,
    user_id BIGINT UNSIGNED NOT NULL,
    role ENUM('owner', 'admin', 'member') NOT NULL,
    from_seq BIGINT UNSIGNED NOT NULL,
    to_seq BIGINT UNSIGNED NULL,
    joined_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    left_at DATETIME(6) NULL,
    active_user_id BIGINT UNSIGNED GENERATED ALWAYS AS (IF(left_at IS NULL, user_id, NULL)) STORED,
    PRIMARY KEY (id),
    UNIQUE KEY active_membership (conversation_id, active_user_id),
    KEY membership_visibility (tenant_id, app_id, user_id, conversation_id, from_seq, to_seq),
    KEY membership_conversation_scope (tenant_id, app_id, conversation_id),
    CONSTRAINT membership_conversation FOREIGN KEY (tenant_id, app_id, conversation_id) REFERENCES conversations (tenant_id, app_id, id),
    CONSTRAINT membership_user FOREIGN KEY (tenant_id, app_id, user_id) REFERENCES users (tenant_id, app_id, id),
    CONSTRAINT membership_range CHECK (from_seq >= 1 AND ((left_at IS NULL) = (to_seq IS NULL)) AND (to_seq IS NULL OR to_seq >= from_seq - 1))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE messages (
    tenant_id BIGINT UNSIGNED NOT NULL,
    app_id BIGINT UNSIGNED NOT NULL,
    conversation_id BIGINT UNSIGNED NOT NULL,
    seq BIGINT UNSIGNED NOT NULL,
    sender_id BIGINT UNSIGNED NOT NULL,
    sender_device_id BIGINT UNSIGNED NOT NULL,
    client_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    payload_hash BINARY(32) NOT NULL,
    body TEXT NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (conversation_id, seq),
    UNIQUE KEY message_client (sender_id, sender_device_id, client_id),
    KEY message_conversation_scope (tenant_id, app_id, conversation_id),
    KEY message_sender (tenant_id, app_id, sender_id),
    KEY message_sender_device (tenant_id, app_id, sender_device_id),
    CONSTRAINT message_conversation FOREIGN KEY (tenant_id, app_id, conversation_id) REFERENCES conversations (tenant_id, app_id, id),
    CONSTRAINT message_sender FOREIGN KEY (tenant_id, app_id, sender_id) REFERENCES users (tenant_id, app_id, id),
    CONSTRAINT message_device FOREIGN KEY (tenant_id, app_id, sender_device_id) REFERENCES devices (tenant_id, app_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE message_outbox (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    conversation_id BIGINT UNSIGNED NOT NULL,
    seq BIGINT UNSIGNED NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    dispatched_at DATETIME(6) NULL,
    PRIMARY KEY (id),
    UNIQUE KEY outbox_message (conversation_id, seq),
    KEY outbox_pending (dispatched_at, id),
    CONSTRAINT outbox_message FOREIGN KEY (conversation_id, seq) REFERENCES messages (conversation_id, seq)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
