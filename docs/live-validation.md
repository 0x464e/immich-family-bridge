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

The opt-in `remove_unshared_replicas` flow was later validated against Immich 3.2.2 with both an ordinary image and an image with an associated XMP. It removed translated album membership, waited one cycle in `pending_removal`, force-deleted only recipient Immich assets, retained the origin media and XMP, and removed leftover recipient hardlinks after exact inode checks. The bridge was force-killed after persisting `pending_removal`; on restart it completed both recipient deletions. Re-adding both origins recreated and imported all replicas, including the XMP association.

This initial validation covered the tested instance and the ordinary JPEG path. Live Photos, motion photos, stacks, edits, videos, public links, and destructive cleanup were not exercised. Validate those separately before extending support.

## Larger delayed-import run (2026-09-18)

On the same disposable Immich 3.2.2 instance, we set Immich's job concurrency to one, generated 1,200 distinct synthetic PNGs (236 MB total), and imported them into one member's source External Library. We added all 1,200 assets to that member's Together album and ran the bridge in active mode with three members. The bridge found the full album through two pages of Immich metadata search. This exposed a pagination bug: Immich returned `nextPage` while the client had only followed `nextCursor`. The adapter now follows either response field and rejects repeated assets or stalled pagination.

The bridge prepared at most 500 new replicas, looked up at most 500 pending imports, and audited at most 200 ready replicas per member and album in each cycle. It requested one recipient library scan per member initially and one more per member after the 90-second scan cooldown; pending imports resolved over later cycles. Album additions used batches of at most 100. The 2,400 new recipient replicas reached `ready` in about three minutes. All three Together albums then contained 1,204 assets, including the four earlier test photos. All 2,408 recipient files (old and new) existed with the same inode as their source and a link count of three. The bridge emitted no warnings or errors during this run. The test used disposable media only; it does not establish a throughput guarantee for a production server or a 14,000-asset album.

Restarting the test bridge during an audit exposed shutdown warnings from an in-flight cycle after SQLite had closed. The shutdown path now cancels that cycle, waits for the polling worker and HTTP server, and then closes SQLite. A regression test checks that cancellation produces no warning and that a later cycle can resume with the stored mappings. The temporary one-job Immich concurrency limits were restored after the load test.

## Abrupt-stop recovery (2026-09-18)

We added another 600 distinct synthetic PNGs to the disposable source library and then to one member's Together album. While the bridge was creating recipient links, we sent the container `SIGKILL`, bypassing graceful shutdown. SQLite immediately held 269 `pending_import` replicas and one `pending_link` replica; `PRAGMA quick_check` returned `ok`. We restarted the same container with its existing state volume. It completed the remaining links, scans, imports, and album additions without warning or error logs. All three Together albums ended with 1,804 assets. The database held 5,412 distinct ready asset mappings, and all 3,608 recipient files existed and matched their sources by inode. The database still passed its integrity check. The test confirms process-kill recovery on this storage setup; it cannot simulate every effect of a physical power loss or damaged storage.

## XMP sidecars (2026-09-19)

We created disposable image and `.xmp` pairs in an External Library and hardlinked both into a recipient library. When both files existed before the recipient scan, Immich associated the sidecar during import and applied its rating. Device and inode checks confirmed that the source and recipient XMP were the same underlying file.

We then imported another image without an XMP, created and linked its sidecar afterward, and confirmed that an ordinary External Library scan did not associate the new file. Immich 3.2.2's administrator sidecar discovery job associated the exact XMP path for both the source and recipient and applied its rating. The bridge now records media and sidecar components separately, requests that discovery job only when an imported recipient lacks the expected association, polls `/asset-files` for the exact path, and issues a targeted metadata refresh when the already-associated XMP later changes. Fake-backend tests cover retry and idempotence. Only Immich-associated XMP sidecars are supported; other sidecar formats and coupled media remain outside this validation.

## Together as the authoritative shared set

The Together membership refactor was exercised on the disposable Immich 3.2.2
instance with three accounts and its existing 1,805-asset shared library. A test
asset was present in both Together and Highlights. Removing it from Together
removed it from both albums for all members and deleted both recipient replicas,
while retaining the origin. Adding the origin back through Highlights populated
Together and Highlights for all three accounts. Removing it from a recipient's
Highlights removed only Highlights membership and preserved Together sharing.

The bridge was force-killed after the global removal decision reached SQLite.
After restart, removal completed without resurrecting membership from Highlights.
A subsequent Highlights addition restored sharing, and the fixture was left
shared in both albums. Unit tests also cover legacy secondary-only membership
backfill, conflicting edits, a failed album read, a failed remote write, dry-run
promotion without writes, and delaying secondary album additions until the
recipient's Together membership has been written.

## Transparent cross-account links (2026-09-20)

Using the same disposable Immich 3.2.2 instance, we rebuilt the bridge from
the current checkout and placed a short-lived Traefik 3.5.3 proxy in front of
it. An authenticated User 2 request for a known User 1 `/photos/<uuid>` link
received a `307` to User 2's ready replica, retained its query string, and
followed the relative redirect to Immich's normal photo-page response. The
target link did not redirect again. The reverse User 1-to-User 2 request also
translated correctly, while an unknown UUID and an invalid Immich session
passed through with the resolver's `204` result. This exposed two Traefik
configuration requirements now reflected in the deployment: express GET and
HEAD as separate `Method` matchers, and set `preserveLocationHeader` so a
relative bridge redirect is not resolved against the private ForwardAuth URL.

The same rebuilt bridge and short-lived proxy were then tested with the
bridge-managed Together albums. An authenticated User 2 request for User 1's
`/albums/<uuid>?from=traefik-e2e` link received a relative `307` to User 2's
matching mirror album with the query string unchanged; following it reached
Immich with HTTP 200. Direct bridge checks also verified the reverse direction,
that a member's own mirror-album link returns `204`, and that an unregistered
album returns `204`. The album resolver queries only `album_replicas`, so it
does not translate ordinary Immich album links.

## Bounded reconciliation pass (2026-09-21)

The refactored bridge was built from this branch and run only against the
disposable three-account Immich 3.2.2 setup, reusing its existing 1,807-asset
Together albums. The first complete discovery took about 4.5 seconds with no
errors. Subsequent work-only passes, including 25 ready-asset audits per member,
took about 2.1 seconds at the test instance's 10-second work interval. The work
passes did not search or patch albums. The disposable source and recipient
libraries remained separate from production throughout.

A live stack test caught a difference from the fake backend: Immich's album
metadata search returned `stack: null` even after two existing source assets
were stacked. The refactor now calls `GET /stacks` once per member during full
discovery, filters out bridge-created recipient stacks, and processes source
stacks in bounded batches. Rebuilding the disposable bridge discovered the
retroactive source stack and created recipient stacks on both other accounts
with the expected primary-asset mapping. Deleting the source stack and forcing
a full reconcile removed both recipient stacks; all three accounts then had
zero stacks. The test used existing disposable images, not production media.

This validates the discovery/work split and one create/delete stack sequence;
it is not a throughput or crash-recovery guarantee for a 25,000-asset library.
