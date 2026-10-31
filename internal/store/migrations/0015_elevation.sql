-- A fresh passkey assertion within a session (§13.10): until elevated_until,
-- that session may also widen who may do what — issue a token, add an
-- identity, raise a grant — which an ordinary session may only tighten.
-- The writer is internal/store/identity.go (Elevate); internal/authn reads
-- it into the request's Viewer.
--
-- On the session's row rather than in memory: a restart or a second
-- process sees the same answer, and nothing else needs a lifetime of its
-- own (logging out revokes the row, and with it the elevation).
--
-- Only a session can be elevated: a token is not a person at a browser,
-- whatever its scopes.
ALTER TABLE credentials
    ADD COLUMN elevated_until timestamptz,
    ADD CONSTRAINT credentials_elevated_session CHECK (elevated_until IS NULL OR kind = 'session');
