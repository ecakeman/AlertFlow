#!/bin/sh
set -eu
export PGPASSWORD="${POSTGRES_PASSWORD}"
psql -h postgres -U "${POSTGRES_USER}" -d postgres -v ON_ERROR_STOP=1 <<'SQL'
SELECT format('CREATE ROLE gateway_app LOGIN PASSWORD %L', 'gateway')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'gateway_app')\gexec
SELECT format('CREATE ROLE knowledge_app LOGIN PASSWORD %L', 'knowledge')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'knowledge_app')\gexec
SELECT 'CREATE DATABASE knowledge OWNER knowledge_app'
WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'knowledge')\gexec
GRANT CONNECT ON DATABASE alertflow TO gateway_app;
GRANT CREATE ON DATABASE alertflow TO gateway_app;
SQL
psql -h postgres -U "${POSTGRES_USER}" -d knowledge -v ON_ERROR_STOP=1 <<'SQL'
CREATE EXTENSION IF NOT EXISTS vector;
GRANT ALL ON SCHEMA public TO knowledge_app;
SQL
