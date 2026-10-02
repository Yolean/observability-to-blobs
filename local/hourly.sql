-- Views over the hourly tier under the session variable hourly_prefix
-- (s3://<bucket>/hourly/<cluster>): one object per table per hour,
-- namespace-sorted, and the pods index. Creating a view binds its glob,
-- so read this file only when the tier has objects. union_by_name: raw1
-- changes are additive, so an older hour lacks the newer columns, which
-- read as NULL; without it the first object's columns win, and a later
-- object without one of them fails the query.
CREATE OR REPLACE VIEW hourly_logs AS
  SELECT * FROM read_parquet(getvariable('hourly_prefix') || '/logs/*/*.parquet', filename=true, union_by_name=true);
CREATE OR REPLACE VIEW hourly_metrics AS
  SELECT * FROM read_parquet(getvariable('hourly_prefix') || '/metrics/*/*.parquet', filename=true, union_by_name=true);
CREATE OR REPLACE VIEW hourly_podindex AS
  SELECT * FROM read_parquet(getvariable('hourly_prefix') || '/podindex/*/*.parquet', filename=true, union_by_name=true);
CREATE OR REPLACE VIEW pods AS
  SELECT * FROM read_parquet(getvariable('hourly_prefix') || '/pods/*/*.parquet', filename=true, union_by_name=true);
