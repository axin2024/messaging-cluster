ALTER TABLE message_outbox
    ADD kind ENUM('message', 'read', 'retraction') NOT NULL DEFAULT 'message',
    ADD recipient_user_id BIGINT UNSIGNED NOT NULL DEFAULT 0,
    ADD origin_device_id BIGINT UNSIGNED NULL,
    ADD live_delivery BOOLEAN NOT NULL DEFAULT FALSE,
    DROP INDEX outbox_message,
    DROP INDEX outbox_pending,
    ADD UNIQUE KEY outbox_event (conversation_id, seq, kind, recipient_user_id),
    ADD KEY outbox_delivery_queue (live_delivery, dispatched_at, id),
    ADD CONSTRAINT outbox_recipient CHECK ((kind = 'read') = (recipient_user_id > 0));

-- Existing activity is already available through sync. Start live routing with new events.
ALTER TABLE message_outbox ALTER COLUMN live_delivery SET DEFAULT TRUE;

ALTER TABLE messages ADD KEY message_scope_time (tenant_id, app_id, created_at);

CREATE TABLE delivery_attempts (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    outbox_id BIGINT UNSIGNED NOT NULL,
    tenant_id BIGINT UNSIGNED NOT NULL,
    app_id BIGINT UNSIGNED NOT NULL,
    node_id BIGINT UNSIGNED NOT NULL,
    device_id BIGINT UNSIGNED NOT NULL,
    epoch BIGINT UNSIGNED NOT NULL,
    membership_id BIGINT UNSIGNED NOT NULL,
    state ENUM('queued', 'offered', 'sent', 'acked', 'resync') NOT NULL DEFAULT 'queued',
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    sent_at DATETIME(6) NULL,
    finished_at DATETIME(6) NULL,
    PRIMARY KEY (id),
    UNIQUE KEY delivery_event_device (outbox_id, device_id, epoch),
    KEY delivery_node_queue (node_id, state, id),
    KEY delivery_scope_time (tenant_id, app_id, created_at, node_id),
    KEY delivery_expiry (state, created_at, id),
    CONSTRAINT delivery_outbox FOREIGN KEY (outbox_id) REFERENCES message_outbox (id),
    CONSTRAINT delivery_connection FOREIGN KEY (device_id, epoch) REFERENCES device_connections (device_id, epoch),
    CONSTRAINT delivery_membership FOREIGN KEY (membership_id) REFERENCES memberships (id),
    CONSTRAINT delivery_node FOREIGN KEY (node_id) REFERENCES gateway_nodes (id)
) ENGINE=InnoDB;

CREATE TABLE tenant_message_limits (
    tenant_id BIGINT UNSIGNED NOT NULL,
    messages_per_second INT UNSIGNED NOT NULL,
    PRIMARY KEY (tenant_id),
    CONSTRAINT tenant_limit_owner FOREIGN KEY (tenant_id) REFERENCES tenants (id),
    CONSTRAINT tenant_limit_range CHECK (messages_per_second BETWEEN 1 AND 10000)
) ENGINE=InnoDB;

CREATE TABLE tenant_send_buckets (
    tenant_id BIGINT UNSIGNED NOT NULL,
    second_epoch BIGINT UNSIGNED NOT NULL,
    accepted INT UNSIGNED NOT NULL,
    PRIMARY KEY (tenant_id, second_epoch),
    KEY send_bucket_age (second_epoch),
    CONSTRAINT send_bucket_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id)
) ENGINE=InnoDB;
