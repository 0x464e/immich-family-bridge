CREATE TABLE album_deletions(
    logical_album_id TEXT PRIMARY KEY REFERENCES logical_albums(id),
    record TEXT NOT NULL
);
CREATE TABLE album_marker_observations(
    logical_album_id TEXT NOT NULL REFERENCES logical_albums(id),
    member_id TEXT NOT NULL REFERENCES members(id),
    marker_user_id TEXT NOT NULL,
    PRIMARY KEY(logical_album_id,member_id)
);
