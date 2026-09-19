CREATE TABLE album_cover_observations(
    logical_album_id TEXT NOT NULL REFERENCES logical_albums(id),
    member_id TEXT NOT NULL REFERENCES members(id),
    immich_asset_id TEXT NOT NULL DEFAULT '',
    PRIMARY KEY(logical_album_id, member_id)
);
