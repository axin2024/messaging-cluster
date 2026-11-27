ALTER TABLE partner_events ADD COLUMN detail JSON NULL,
    ADD KEY organization_events (link_id,actor_app,id);
ALTER TABLE partner_links
    ADD COLUMN host_frozen BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN peer_frozen BOOLEAN NOT NULL DEFAULT FALSE;
UPDATE partner_links SET host_frozen=TRUE,peer_frozen=TRUE WHERE state='frozen';
