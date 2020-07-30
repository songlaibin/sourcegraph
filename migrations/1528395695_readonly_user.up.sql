
BEGIN;

CREATE USER sgreader with PASSWORD 'sgreader';
GRANT CONNECT ON DATABASE sourcegraph TO sgreader;
GRANT USAGE ON SCHEMA public to sgreader;
GRANT SELECT ON ALL TABLES IN SCHEMA public to sgreader;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO sgreader;

COMMIT;
