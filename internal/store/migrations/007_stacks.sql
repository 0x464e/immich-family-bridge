CREATE TABLE source_stacks(
    source_member_id TEXT NOT NULL REFERENCES members(id),
    source_stack_id TEXT NOT NULL,
    primary_immich_asset_id TEXT NOT NULL,
    PRIMARY KEY(source_member_id, source_stack_id)
);
CREATE TABLE stack_replicas(
    source_member_id TEXT NOT NULL,
    source_stack_id TEXT NOT NULL,
    member_id TEXT NOT NULL REFERENCES members(id),
    immich_stack_id TEXT,
    signature TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL,
    error TEXT,
    PRIMARY KEY(source_member_id, source_stack_id, member_id),
    FOREIGN KEY(source_member_id, source_stack_id) REFERENCES source_stacks(source_member_id, source_stack_id) ON DELETE CASCADE
);
