ALTER TABLE partner_invitations ADD COLUMN retention_days SMALLINT UNSIGNED NOT NULL DEFAULT 0;
ALTER TABLE shared_messages ADD COLUMN retained_until DATETIME(6) NULL,
    ADD KEY expiring_messages (retained_until,hidden_at);
