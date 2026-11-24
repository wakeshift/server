-- The database docker-compose.yml's Postgres starts with, owned by an
-- ordinary role, so row-level security applies: a superuser or a BYPASSRLS
-- role bypasses it, and the server refuses to run as either. The role may
-- create roles (CREATEROLE), as managed Postgres services' users can: a
-- migration makes a read-only role for an admin service.
--
-- The password is for this local database only (docker-compose.yml
-- publishes Postgres on 127.0.0.1); use your own anywhere else.
CREATE ROLE wakeshift LOGIN PASSWORD 'wakeshift' NOSUPERUSER NOBYPASSRLS CREATEROLE;
CREATE DATABASE wakeshift OWNER wakeshift;
