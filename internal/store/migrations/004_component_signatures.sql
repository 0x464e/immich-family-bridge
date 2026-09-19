ALTER TABLE asset_replica_files ADD COLUMN source_size INTEGER NOT NULL DEFAULT 0;
ALTER TABLE asset_replica_files ADD COLUMN source_mtime_ns INTEGER NOT NULL DEFAULT 0;
