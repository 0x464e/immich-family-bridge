ALTER TABLE logical_albums ADD COLUMN system_key TEXT;
CREATE UNIQUE INDEX logical_albums_system_key_unique ON logical_albums(system_key) WHERE system_key IS NOT NULL;
