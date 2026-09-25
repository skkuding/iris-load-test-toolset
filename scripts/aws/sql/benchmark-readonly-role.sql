-- Bootstrap the read-only benchmark database role.
--
-- Executed by scripts/aws/bootstrap-benchmark-db-role.sh as the database
-- master user. The script supplies these psql variables:
--   :'ro_user'      read-only role name
--   :'ro_password'  read-only role password (from Secrets Manager)
--   :'db_name'      benchmark database name
--
-- This file is idempotent and contains no credentials. It grants only read
-- privileges: CONNECT, USAGE, and SELECT, and forces read-only transactions
-- for the role's sessions.

-- Create the role only when it does not already exist. \gexec runs the row as SQL.
SELECT format('CREATE ROLE %I LOGIN', :'ro_user')
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = :'ro_user')\gexec

-- Always (re)set the password from the managed secret.
ALTER ROLE :"ro_user" WITH LOGIN PASSWORD :'ro_password';

-- Enforce read-only sessions and bound runaway queries regardless of caller.
ALTER ROLE :"ro_user" SET default_transaction_read_only = on;
ALTER ROLE :"ro_user" SET statement_timeout = '15min';

-- Minimum privileges for a read-only judge/benchmark data path.
GRANT CONNECT ON DATABASE :"db_name" TO :"ro_user";
GRANT USAGE ON SCHEMA public TO :"ro_user";
GRANT SELECT ON ALL TABLES IN SCHEMA public TO :"ro_user";
GRANT SELECT ON ALL SEQUENCES IN SCHEMA public TO :"ro_user";

-- Cover tables created after this bootstrap by the master user.
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO :"ro_user";
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON SEQUENCES TO :"ro_user";

-- Defensively remove any inherited or previously granted write privileges.
REVOKE INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER
  ON ALL TABLES IN SCHEMA public FROM :"ro_user";
