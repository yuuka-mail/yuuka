-- M2: sent copies become messages (§4.6). The link between a message and
-- the outbox rows it was sent by is the fold key, not a stored id: the
-- message whose content_hash is raw_objects.content_hash of outbox.raw_id.
-- Reading it from the message side, which every list row does to tell
-- our own mail apart and to decide visibility (§13.8), needs this index.
CREATE INDEX raw_objects_content_hash ON raw_objects (content_hash) WHERE content_hash IS NOT NULL;

-- Inbound In-Reply-To / References are matched against both ids an
-- attempt records (§10.4.1): message_id (indexed by 0005) and the
-- platform's provider_id. For Cloudflare the two are equal (measured
-- 2026-10-31); provider_id still counts for exits that report only an id
-- of their own.
CREATE INDEX send_attempts_provider_id ON send_attempts (provider_id) WHERE provider_id IS NOT NULL;
