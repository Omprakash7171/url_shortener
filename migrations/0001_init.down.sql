-- 0001_init.down.sql
-- Reverts 0001_init.up.sql.

DROP TABLE IF EXISTS idempotency_records;
DROP TABLE IF EXISTS analytics_daily;
DROP TABLE IF EXISTS urls;