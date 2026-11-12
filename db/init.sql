-- PatchMan Database Initialization
-- Runs automatically on first PostgreSQL container start

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS "pg_trgm";
CREATE EXTENSION IF NOT EXISTS "unaccent";

-- Tables are managed by GORM AutoMigrate at application startup.
-- No manual DDL required.
