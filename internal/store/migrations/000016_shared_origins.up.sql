CREATE TABLE shared_message_origins (
    channel_id BIGINT UNSIGNED NOT NULL,
    seq BIGINT UNSIGNED NOT NULL,
    source_conversation BIGINT UNSIGNED NOT NULL,
    source_seq BIGINT UNSIGNED NOT NULL,
    actor_tenant BIGINT UNSIGNED NOT NULL,
    actor_app BIGINT UNSIGNED NOT NULL,
    actor_user BIGINT UNSIGNED NOT NULL,
    published_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (channel_id,seq),
    KEY legacy_source (source_conversation,source_seq),
    FOREIGN KEY (channel_id,seq) REFERENCES shared_messages(channel_id,seq),
    FOREIGN KEY (source_conversation,source_seq) REFERENCES messages(conversation_id,seq),
    FOREIGN KEY (actor_tenant,actor_app,actor_user) REFERENCES users(tenant_id,app_id,id)
) ENGINE=InnoDB;
