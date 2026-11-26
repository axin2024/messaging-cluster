CREATE TABLE membership_operations (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id BIGINT UNSIGNED NOT NULL,
    app_id BIGINT UNSIGNED NOT NULL,
    conversation_id BIGINT UNSIGNED NOT NULL,
    actor_id BIGINT UNSIGNED NOT NULL,
    device_id BIGINT UNSIGNED NOT NULL,
    actor_role ENUM('owner', 'admin', 'member') NOT NULL,
    client_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL,
    kind ENUM('set', 'remove', 'transfer', 'batch') NOT NULL,
    items JSON NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    completed_at DATETIME(6) NULL,
    PRIMARY KEY (id),
    KEY membership_operations_conversation (tenant_id, app_id, conversation_id, id),
    UNIQUE KEY membership_operations_client (conversation_id, actor_id, client_id),
    KEY membership_operations_actor (tenant_id, app_id, actor_id),
    KEY membership_operations_device (tenant_id, app_id, device_id),
    CONSTRAINT membership_operations_group FOREIGN KEY (tenant_id, app_id, conversation_id) REFERENCES conversations (tenant_id, app_id, id),
    CONSTRAINT membership_operations_actor FOREIGN KEY (tenant_id, app_id, actor_id) REFERENCES users (tenant_id, app_id, id),
    CONSTRAINT membership_operations_device FOREIGN KEY (tenant_id, app_id, device_id) REFERENCES devices (tenant_id, app_id, id),
    CONSTRAINT membership_operations_batch CHECK (
        (kind = 'batch' AND client_id IS NOT NULL) OR (kind <> 'batch' AND client_id IS NULL AND completed_at IS NOT NULL)
    )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
