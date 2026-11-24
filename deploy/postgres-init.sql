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

-- Webhooks (apps/try, docker-compose.yml's profile webhooks) keeps its own
-- database beside the server's, with the same two roles: webhooks owns it
-- and migrates it at startup (TRY_MIGRATE_DATABASE_URL), and webhooks_app,
-- the app's own role (TRY_DATABASE_URL), owns nothing and only reads and
-- writes the tables webhooks makes (its default privileges, below). Only
-- those two may connect to it.
CREATE ROLE webhooks LOGIN PASSWORD 'webhooks' NOSUPERUSER NOBYPASSRLS NOCREATEROLE NOCREATEDB;
CREATE ROLE webhooks_app LOGIN PASSWORD 'webhooks_app' NOSUPERUSER NOBYPASSRLS NOCREATEROLE NOCREATEDB;
CREATE DATABASE webhooks OWNER webhooks;
REVOKE CONNECT, TEMPORARY ON DATABASE webhooks FROM PUBLIC;
GRANT CONNECT ON DATABASE webhooks TO webhooks, webhooks_app;
\connect webhooks
ALTER DEFAULT PRIVILEGES FOR ROLE webhooks IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO webhooks_app;
ALTER DEFAULT PRIVILEGES FOR ROLE webhooks IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO webhooks_app;
