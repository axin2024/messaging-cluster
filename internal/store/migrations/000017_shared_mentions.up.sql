CREATE TABLE shared_message_mentions (
    channel_id BIGINT UNSIGNED NOT NULL,
    seq BIGINT UNSIGNED NOT NULL,
    recipient_tenant BIGINT UNSIGNED NOT NULL,
    recipient_app BIGINT UNSIGNED NOT NULL,
    recipient_user BIGINT UNSIGNED NOT NULL,
    PRIMARY KEY (channel_id,seq,recipient_user),
    KEY mentions_inbox (channel_id,recipient_app,recipient_user,seq),
    FOREIGN KEY (channel_id,seq) REFERENCES shared_messages(channel_id,seq),
    FOREIGN KEY (recipient_tenant,recipient_app,recipient_user) REFERENCES users(tenant_id,app_id,id)
) ENGINE=InnoDB;
