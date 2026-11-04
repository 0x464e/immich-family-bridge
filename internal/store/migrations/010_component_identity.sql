ALTER TABLE asset_replica_files ADD COLUMN source_device INTEGER NOT NULL DEFAULT 0;
ALTER TABLE asset_replica_files ADD COLUMN source_inode INTEGER NOT NULL DEFAULT 0;
