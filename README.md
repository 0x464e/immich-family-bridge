# Immich Family Bridge

Immich Family Bridge mirrors registered albums across separate Immich users. Each member sees their own Immich asset IDs in their own album. Recipient assets are imported from hardlinks in a member-owned External Library. The service keeps logical mappings and album observations in SQLite and reconciles them on a timer or through its internal API.

The service connects to a real Immich API. There is no runnable fake mode. Unit tests use a test client and temporary files. The [live validation record](docs/live-validation.md) describes a disposable Immich 3.2.2 run on an NFS-backed ZFS dataset.

## Prepare Immich and storage

1. Create at least two Immich member accounts. Create one External Library per member, owned by that member. Give each library exactly one import path: `<immich_bridge_root>/families/<family_id>/users/<member_id>/assets`. Library setup and scans require an Immich administrator. Mount the recipient tree read-only into the Immich server.
2. Mount the **same parent filesystem tree once** into the bridge container. Both original media and recipient output must be under that one mount, or `link(2)` can fail with `EXDEV` even if two host paths reside on the same filesystem. The bridge process needs read access to originals and write access to recipient output. Keep SQLite on local storage, outside the media tree.
3. Make a private copy of [config.example.yaml](config.example.yaml). Map each Immich `originalPath` prefix to its path inside the bridge container in `source_mappings`. For example, Immich may return `/data/library/...` for a managed upload while the bridge sees the same file at `/media/immich-upload/library/...`. Add a mapping for every source mount you intend to share. Set `source_root` to the common media mount and `bridge_root` to its recipient subtree.
4. Create member API keys with `user.read`, `asset.read`, `assetFile.read`, `album.create`, `album.read`, `album.update`, `albumAsset.create`, and `albumAsset.delete`. Create a separate administrator key with `user.read`, `library.read`, and `library.update`. Put the keys and a random internal API token in a private env file based on [secrets.env.example](secrets.env.example). Keys are never logged.

The bridge checks member-key identities and each recipient library's owner and import path at startup. It rejects Immich major versions other than 3. Its HTTP adapter is pinned to the [Immich 3.2.2 OpenAPI schema](https://github.com/immich-app/immich/blob/v3.2.2/open-api/immich-openapi-specs.json); validate behavior before adopting a newer release.

## Run

Copy [compose.env.example](compose.env.example), fill in absolute host paths and the Immich Docker network name, then run:

```sh
docker compose --env-file /path/to/bridge-compose.env up --build -d
curl http://127.0.0.1:8081/readyz
```

The Compose service binds to localhost by default. `GET /healthz` checks the process; `GET /readyz` also checks SQLite, Immich connectivity, member identities, and recipient libraries. The internal API requires `Authorization: Bearer <FAMILYBRIDGE_API_TOKEN>`:

- `GET /api/members`, `GET /api/albums`, `GET /api/replicas`, `GET /api/assets/{logicalAssetId}`, and `GET /api/filesystem` inspect mappings and link health.
- `POST /api/albums` registers an existing member album. Send `memberId` and `albumId`; optionally send `replicas`, a map of other member IDs to existing album IDs. Initial reconciliation takes the union of their assets.
- `PATCH /api/albums/{logicalAlbumId}` updates the canonical name, description, and optional `coverLogicalAssetId`.
- `POST /api/reconcile/dry-run` reports proposed actions without writes. `POST /api/reconcile` runs a cycle.

For example, register an album after authenticating to the bridge API:

```sh
curl -H "Authorization: Bearer $FAMILYBRIDGE_API_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"memberId":"alice","albumId":"<alice-album-id>"}' \
  http://127.0.0.1:8081/api/albums
```

## Reconciliation and limits

The first cycle takes the union of member-owned assets in the registered albums. Later member additions and removals update canonical membership; concurrent additions win over removals. One asset can have references from multiple logical albums. The bridge writes recipient hardlinks at stable paths, scans the recipient's External Library, waits for a unique matching imported asset, then adds that member's Immich asset ID to their album. Immich's `originalPath` search can return prefix matches, so the bridge checks the exact owner, library, and path before storing an ID.

The linker rejects symlinks, path escapes, nonregular sources, conflicting destination inodes, and cross-filesystem links. It never copies as a fallback. Removing the last album reference marks recipient files `pending_removal`; it does not unlink them or delete Immich assets. Source deletion and unsupported coupled media remain visible states. The first media adapter handles ordinary single-file images and videos; Live Photos, motion photos, stacks, sidecars, and edited forms are skipped. Per-user Immich metadata remains separate. Public links are not created.

External Library paths must stay stable because moving an imported file can lose Immich-local metadata. On NFS, rely on explicit library scans and polling instead of file watching. Before attaching production media, confirm UID/GID, NFS hardlink permissions, mount topology, import paths, and a successful disposable end-to-end cycle. The bridge application never modifies or unlinks an Immich-managed original.

Immich may move a freshly uploaded original into a storage-template path after the bridge first sees it. The bridge updates its stored origin path only when the new file has the same inode as the old source or existing recipient links. A different-inode move remains visible for review; recipient paths never change.

## Verify and release

```sh
go test ./...
go vet ./...
goreleaser check
goreleaser release --snapshot --skip=archive,docker --clean
```

The local Git repository uses conventional commits on `master`, release-please, and GoReleaser. A GitHub release publishes only the `0x464e/immich-family-bridge` multi-platform Docker Hub image; Linux binaries are intermediate build inputs. The first proposed version is `v0.1.0`. Configure `ACTIONS_PAT` and `DOCKER_PAT` before enabling those workflows on GitHub. No Git remote is configured and nothing has been pushed.
