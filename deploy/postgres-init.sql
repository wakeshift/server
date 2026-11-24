-- The database docker-compose.yml's Postgres starts with, and its two
-- roles, both ordinary, so row-level security applies: a superuser or a
-- BYPASSRLS role bypasses it, and the server refuses to run as either.
--
-- wakeshift owns the database and its tables and migrates them
-- (WAKESHIFT_MIGRATE_DATABASE_URL). It may create roles (CREATEROLE), as
-- managed Postgres services' users can: a migration makes a read-only role
-- for an admin service.
--
-- wakeshift_server is the server's own role (DATABASE_URL): it owns
-- nothing, and the owner grants it reads and writes at each start, so the
-- server can't alter tables, turn off row-level security or rewrite the
-- audit log.
--
-- The passwords are for this local database only (docker-compose.yml
-- publishes Postgres on 127.0.0.1); use your own anywhere else.
CREATE ROLE wakeshift LOGIN PASSWORD 'wakeshift' NOSUPERUSER NOBYPASSRLS CREATEROLE;
CREATE ROLE wakeshift_server LOGIN PASSWORD 'wakeshift_server' NOSUPERUSER NOBYPASSRLS NOCREATEROLE NOCREATEDB;
CREATE DATABASE wakeshift OWNER wakeshift;
