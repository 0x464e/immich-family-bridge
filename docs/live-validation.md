# Disposable Immich validation

Validated on 2026-09-17 against Immich 3.2.2, three member accounts, four ordinary JPEG uploads, three recipient External Libraries, and an NFSv4.2 mount backed by a disposable ZFS dataset. No production Immich media or database was mounted.

The Immich server mounted the recipient tree read-only. The bridge mounted the common media parent once at `/media`, with the managed upload tree mapped from Immich's `/data` original paths. SQLite stayed on local storage. Recipient files had the same device and inode as their origins and a link count of three.

Observed API behavior:

- Scoped member keys could read identity, assets, album membership, and asset files, and edit owned albums. Library management returned HTTP 403 to regular users. A separate administrator key could read and scan recipient libraries.
- `POST /search/metadata` with `albumIds` returned album members. Its `originalPath` filter also returned a prefix match; the bridge's exact owner, library, and path check is necessary.
- An explicit library scan made a new hardlinked recipient asset searchable after a short delay. Pending imports resolved on later reconciliation cycles.
- Managed uploads with the storage template enabled returned paths under `/data/library/...`; the source mapping resolved them to the bridge's mounted media tree.
- One upload first appeared under `/data/upload/...` and later moved to `/data/library/...`. The bridge followed the move only after confirming that the new source and its recipient hardlinks had the same inode. A different-inode path change remains an error.

End-to-end reconciliation checked the initial union of three owned photos into each member's “Together” album, a second album referencing one photo, removal and re-addition from different members, a newly uploaded fourth photo, retained recipient files and `pending_removal` after the last sharing reference was removed, and restoration when re-added. A brief Immich outage made `/readyz` return 503; it returned 200 after Immich recovered. Restarting the bridge preserved four logical assets, two logical albums, twelve ready asset mappings, eight recipient files with matching source inodes, and an empty dry run.

This validation covers the tested instance and the ordinary JPEG path. Live Photos, motion photos, sidecars, stacks, edits, videos, public links, and destructive cleanup were not exercised. Validate those separately before extending support.
