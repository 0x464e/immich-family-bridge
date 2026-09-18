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

## Larger delayed-import run (2026-09-18)

On the same disposable Immich 3.2.2 instance, we set Immich's job concurrency to one, generated 1,200 distinct synthetic PNGs (236 MB total), and imported them into one member's source External Library. We added all 1,200 assets to that member's Together album and ran the bridge in active mode with three members. The bridge found the full album through two pages of Immich metadata search. This exposed a pagination bug: Immich returned `nextPage` while the client had only followed `nextCursor`. The adapter now follows either response field and rejects repeated assets or stalled pagination.

The bridge prepared at most 500 new replicas, looked up at most 500 pending imports, and audited at most 200 ready replicas per member and album in each cycle. It requested one recipient library scan per member initially and one more per member after the 90-second scan cooldown; pending imports resolved over later cycles. Album additions used batches of at most 100. The 2,400 new recipient replicas reached `ready` in about three minutes. All three Together albums then contained 1,204 assets, including the four earlier test photos. All 2,408 recipient files (old and new) existed with the same inode as their source and a link count of three. The bridge emitted no warnings or errors during this run. The test used disposable media only; it does not establish a throughput guarantee for a production server or a 14,000-asset album.

Restarting the test bridge during an audit exposed shutdown warnings from an in-flight cycle after SQLite had closed. The shutdown path now cancels that cycle, waits for the polling worker and HTTP server, and then closes SQLite. A regression test checks that cancellation produces no warning and that a later cycle can resume with the stored mappings. The temporary one-job Immich concurrency limits were restored after the load test.

## Abrupt-stop recovery (2026-09-18)

We added another 600 distinct synthetic PNGs to the disposable source library and then to one member's Together album. While the bridge was creating recipient links, we sent the container `SIGKILL`, bypassing graceful shutdown. SQLite immediately held 269 `pending_import` replicas and one `pending_link` replica; `PRAGMA quick_check` returned `ok`. We restarted the same container with its existing state volume. It completed the remaining links, scans, imports, and album additions without warning or error logs. All three Together albums ended with 1,804 assets. The database held 5,412 distinct ready asset mappings, and all 3,608 recipient files existed and matched their sources by inode. The database still passed its integrity check. The test confirms process-kill recovery on this storage setup; it cannot simulate every effect of a physical power loss or damaged storage.
