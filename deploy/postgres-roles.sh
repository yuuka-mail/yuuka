#!/bin/sh
# Database roles for yuuka; mounted into the postgres container as
# /docker-entrypoint-initdb.d/10-yuuka-roles.sh. The image runs it once,
# when it creates an empty data directory. It is idempotent, so running it
# again repairs the grants, e.g. after a restore into a recreated database:
#
#   docker compose exec postgres sh /docker-entrypoint-initdb.d/10-yuuka-roles.sh
#
# Three login roles, none a superuser (docs/data-model.md, "数据库角色"):
#
#   yuuka_owner   owns the database and its public schema; yuuka connects
#                 as it only to apply migrations (YUUKA_DATABASE_OWNER_URL).
#                 pg_trgm is a trusted extension, so owning the database is
#                 enough to create it.
#   yuuka_app     everything else yuuka does (YUUKA_DATABASE_URL): rows in
#                 the owner's tables and its sequences, no DDL. Default
#                 privileges extend that to tables later migrations create.
#   yuuka_backup  pg_dump in the backup service: reads everything, writes
#                 nothing (pg_read_all_data), so the one container that runs
#                 unattended on a timer cannot change data even if it is
#                 compromised.
#
# The superuser (POSTGRES_USER) is for initdb and maintenance only; no
# service connects as it. Passwords come from the container's environment
# (compose.yaml). Locally, over the socket, the image trusts every role,
# which is what makes this script and `pg_restore -U yuuka_owner` work
# without one.
#
# No `set -eu`: the image sources rather than runs this file when it is
# not executable, and the options would leak into its entrypoint. The
# checks below fail either way, and psql's status is the script's.

: "${POSTGRES_DB:?}" "${YUUKA_DB_OWNER_PASSWORD:?}" "${YUUKA_DB_APP_PASSWORD:?}" "${YUUKA_DB_BACKUP_PASSWORD:?}"

psql -v ON_ERROR_STOP=1 --no-psqlrc --username "${POSTGRES_USER:-postgres}" --dbname "$POSTGRES_DB" \
	-v db="$POSTGRES_DB" \
	-v owner_password="$YUUKA_DB_OWNER_PASSWORD" \
	-v app_password="$YUUKA_DB_APP_PASSWORD" \
	-v backup_password="$YUUKA_DB_BACKUP_PASSWORD" <<'SQL'
SELECT format('CREATE ROLE %I LOGIN', r)
FROM unnest(ARRAY['yuuka_owner', 'yuuka_app', 'yuuka_backup']) AS r
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r)
\gexec
ALTER ROLE yuuka_owner  LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE PASSWORD :'owner_password';
ALTER ROLE yuuka_app    LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE PASSWORD :'app_password';
ALTER ROLE yuuka_backup LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE PASSWORD :'backup_password';
GRANT pg_read_all_data TO yuuka_backup;

ALTER DATABASE :"db" OWNER TO yuuka_owner;
ALTER SCHEMA public OWNER TO yuuka_owner;
-- Only these roles may connect, and nobody but the owner may create
-- anything: no temporary tables, no objects in public.
REVOKE ALL ON DATABASE :"db" FROM PUBLIC;
GRANT CONNECT ON DATABASE :"db" TO yuuka_app, yuuka_backup;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO yuuka_app, yuuka_backup;

-- Rows, not structure. Default privileges cover what the owner creates
-- from now on; the GRANTs cover what it already has (a no-op on a new
-- database, the repair after a restore).
ALTER DEFAULT PRIVILEGES FOR ROLE yuuka_owner IN SCHEMA public
	GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO yuuka_app;
ALTER DEFAULT PRIVILEGES FOR ROLE yuuka_owner IN SCHEMA public
	GRANT USAGE ON SEQUENCES TO yuuka_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO yuuka_app;
GRANT USAGE ON ALL SEQUENCES IN SCHEMA public TO yuuka_app;
SQL
