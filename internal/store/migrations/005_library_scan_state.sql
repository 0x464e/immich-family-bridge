CREATE TABLE library_scan_state(
    member_id TEXT PRIMARY KEY REFERENCES members(id),
    requested_at_ns INTEGER NOT NULL,
    baseline_refreshed_at TEXT NOT NULL DEFAULT ''
);
