# Immich Family Bridge

Immich Family Bridge keeps selected albums in sync across separate [Immich](https://immich.app/) accounts. Each member gets their own Immich asset IDs in their own albums and timeline. The bridge creates recipient files with hardlinks, imports them through member-owned [External Libraries](https://docs.immich.app/features/libraries/), and stores the mappings in SQLite. It neither copies media nor accesses Immich's database.

**Project status:** This is an early release. It was exercised end to end with three accounts, ordinary JPEGs, and a 1,200-image delayed-import batch on a disposable Immich 3.2.2 instance backed by an NFS-mounted ZFS dataset. It has not been validated against a 14,000-asset batch, and video and coupled media have not had live end-to-end testing. See the [validation record](docs/live-validation.md) and [current limits](#current-limits-and-production-pilot) before connecting important media.

## How it works

1. An administrator gives each member a dedicated External Library with one import path pointing to that member's bridge directory. The bridge also receives one API key per member and a separate administrator key for library scans.
2. The bridge automatically manages one catch-all album for every member, using the configured `together_album_name` (default: “Together”). It adopts an existing owned album with that exact name when the match is unique. On the first active cycle it creates any missing member albums. You can register other albums separately; their registered source supplies the initial name, description, and cover.
3. In active mode, each poll reads the registered albums. It records a logical asset for each newly added, member-owned source asset and maps that asset to every member's Immich asset ID. Initial membership is the union of assets in the attached albums. Later additions and removals update logical membership; an addition wins if members add and remove the same asset concurrently.
4. For each recipient, the bridge hardlinks the source into a stable path under `families/<family>/users/<member>/assets/`. It asks Immich to scan that member's External Library, waits for the import, and accepts its ID only when the search finds exactly one asset with the expected owner, library, and full path. It then adds that ID to the recipient album.
5. SQLite stores asset and album mappings, observed membership, sharing references, and pending states. Repeated cycles resume after an import delay, API outage, or bridge restart. Several albums can reference the same logical asset without creating more recipient files.

Recipient assets appear in each owner's normal Immich timeline because they are External Library assets. Each account's favorites, edits, people labels, and other Immich-local metadata remain independent. Album names and descriptions are managed through the bridge API; direct edits to replica albums are overwritten on reconciliation.

## Requirements

- A reachable Immich **3.x** server. The HTTP adapter is tested against the [Immich 3.2.2 OpenAPI schema](https://github.com/immich-app/immich/blob/v3.2.2/open-api/immich-openapi-specs.json) and a disposable 3.2.2 instance. Startup rejects other major versions; validate newer 3.x releases separately.
- At least two Immich accounts with stable user IDs, one dedicated External Library per member, and administrator access to create the libraries. Each library must have exactly one import path and be owned by its member.
- Member API keys with `user.read`, `asset.read`, `assetFile.read`, `album.create`, `album.read`, `album.update`, `albumAsset.create`, and `albumAsset.delete`. The administrator key needs `library.read` and `library.update`. The member scopes and an administrator key with these library scopes were validated on Immich 3.2.2; the administrator key used during validation also had `user.read`, although the bridge does not call a user endpoint with it.
- Linux Docker Engine and Docker Compose for the supplied deployment, or an equivalent container setup. Building from source requires Go 1.25. Published images are available on [Docker Hub](https://hub.docker.com/r/0x464e/immich-family-bridge) for `linux/amd64` and `linux/arm64`.
- A filesystem and mount layout that permits hardlinks from originals to recipient files. Both must be visible beneath **one mount inside the bridge container**. Separate bind mounts can return `EXDEV` even when their host paths are on the same device. An NFS server must permit hardlinks for the bridge's UID/GID.
- Local persistent storage for the bridge's SQLite database, separate from the media tree. Back up this database with the Immich and media data; it contains the identity mappings used to recognize bridge-created assets.

Hardlinks share one underlying file. They save storage but are **not independent copies or backups**: changing bytes through any writable link changes the source. Give the bridge process read permission to originals and write permission only to its recipient tree using filesystem ownership or ACLs. Mount the recipient tree read-only in Immich. Active mode needs a writable common media mount in the bridge container, so host permissions must enforce that boundary.

## Set up storage and Immich

This example shows how the paths relate. Adapt them to your containers:

| Purpose | Example path |
| --- | --- |
| Shared host media tree | `/srv/immich-media` |
| Immich-managed originals on the host | `/srv/immich-media/immich-upload` |
| Original paths reported by Immich | `/data/...` |
| Single bridge container media mount | `/srv/immich-media:/media` |
| Bridge recipient root | `/media/bridge-recipients` |
| Immich recipient mount | `/srv/immich-media/bridge-recipients:/bridge-recipients:ro` |
| Bridge SQLite state | A local directory mounted at `/state` |

The bridge config maps Immich's `/data` paths to `/media/immich-upload` with `source_mappings`. Add a mapping for every other source tree you intend to share. Mappings must resolve to existing directories under `source_root`, outside `bridge_root`. Immich's `originalPath` is a **container path**, not necessarily a host path.

Create the recipient directories before creating libraries, and make them readable by Immich. For family `my-family` and member `alice`, the library's **only** import path is:

~~~text
/bridge-recipients/families/my-family/users/alice/assets
~~~

Repeat for every configured member ID. The bridge checks the library owner and exact import path at startup. Mount the recipient root read-only into the Immich server and any separate worker that scans libraries. Immich documents [External Library paths and scans](https://docs.immich.app/features/libraries/); moving an imported file can cause Immich to treat it as a new asset and lose metadata stored only in Immich. Keep recipient paths stable.

Test the actual deployment mount and UID/GID with a disposable source file before using real media. Two paths on the same host filesystem do not prove that `link(2)` works across the mounts seen by the bridge container.

## Configure and run

1. Copy [config.example.yaml](config.example.yaml) to a private YAML file. Set `family_id`, `together_album_name`, the Immich API URL ending in `/api`, `source_root`, `source_mappings`, `bridge_root`, `immich_bridge_root`, the local SQLite path, and every member's bridge ID, Immich user ID, library ID, and key environment variable. Bridge IDs and the family ID become part of stable paths. The member set cannot be changed after database initialization without an explicit migration.
2. Copy [secrets.env.example](secrets.env.example) to a private environment file. Supply the member keys, administrator key, and a random `FAMILYBRIDGE_API_TOKEN`. Keep both private files out of Git. API keys are not logged.
3. Copy [.env.example](.env.example) to `.env` in your deployment directory. Set absolute host paths, the Immich Docker network name, and `PUID`/`PGID` if the default `1000:1000` cannot read sources and write recipient files and SQLite state. Leave `FAMILYBRIDGE_DRY_RUN=true` and `BRIDGE_MEDIA_MODE=ro` for the first deployment. This file supplies Compose paths and options; the API keys belong in the separate file named by `BRIDGE_ENV_FILE`.
4. Copy [docker-compose.example.yaml](docker-compose.example.yaml) to `compose.yaml` in that directory, review its settings, and start the service:

   ~~~sh
   docker compose --env-file .env up -d
   curl --fail-with-body http://127.0.0.1:6773/readyz
   ~~~

The example uses the published `0x464e/immich-family-bridge:latest` image from Docker Hub and binds the bridge API to `127.0.0.1:6773`. Its `pull_policy: always` checks for a newer image when you recreate the service. Replace `latest` with a specific release tag when you want predictable upgrades.

`GET /healthz` reports whether the HTTP process responds. `GET /readyz` also checks SQLite, Immich reachability, member-key identity, and each recipient library's owner and import path. The service will not start if initial identity checks fail.

### Start with persistent dry-run mode

`FAMILYBRIDGE_DRY_RUN=true` is the default when the environment variable is omitted. The supplied Compose file also mounts the media tree read-only by default. The service stays running: it previews once at startup and on every `poll_interval` (30 seconds by default), logging each proposed action with the album, member, and asset IDs, followed by an action count. Watch it with `docker compose logs -f family-bridge` or the container logs in your deployment UI. On the first dry-run startup, the bridge records its catch-all album in SQLite and logs a proposed album creation for every member whose matching album does not already exist in Immich. For an asset in an attached album, it logs “new source asset found” and “would share asset with member through hardlink and import.” The same actions can appear on later polls because dry-run deliberately does not complete them.

Dry-run does not create Immich albums, add or remove Immich album assets, scan libraries, or create hardlinks. `POST /api/reconcile` returns HTTP 409, and the HTTP adapter and filesystem linker independently reject writes. Automatic catch-all setup and registering other albums write only to the bridge's **local SQLite database** so they can be previewed and used after activation. Updating canonical album metadata through the bridge API also writes only to SQLite until activation. The separate one-shot dry-run HTTP endpoint has been removed; logs are the normal way to inspect the continuous preview.

Confirm the mode with the authenticated `GET /api/status` endpoint before registering an album. Review the logs and exact source path mappings. A preview cannot prove that hardlink permissions, container mount boundaries, or Immich import timing will work; test those with disposable media. To activate, set `FAMILYBRIDGE_DRY_RUN=false` and `BRIDGE_MEDIA_MODE=rw` in the Compose environment, then recreate the bridge container. Its immediate first cycle can then perform writes, so finish the review and backups **before** restarting. Remove any obsolete `dry_run` field from older YAML configurations; mode is controlled only by the environment variable.

## Register albums and inspect status

The internal API requires `Authorization: Bearer <FAMILYBRIDGE_API_TOKEN>` for every `/api/` endpoint. Keep it on localhost or behind an authenticated private network. In active mode, album registration creates missing member albums immediately; polling or a manual reconciliation then fills them. In dry-run mode, registration only saves the mapping in SQLite.

~~~sh
# Set BRIDGE_TOKEN to the private token from your secrets file.
curl --fail-with-body -sS \
  -H "Authorization: Bearer $BRIDGE_TOKEN" \
  http://127.0.0.1:6773/api/status

curl --fail-with-body -sS \
  -H "Authorization: Bearer $BRIDGE_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"memberId":"alice","albumId":"<alice-immich-album-id>"}' \
  http://127.0.0.1:6773/api/albums

# Dry-run previews appear continuously in the service logs. Run this manual
# reconciliation command only after activating the service.
curl --fail-with-body -sS -X POST \
  -H "Authorization: Bearer $BRIDGE_TOKEN" \
  http://127.0.0.1:6773/api/reconcile
~~~

`POST /api/albums` also accepts `"replicas":{"bob":"<bob-album-id>"}` to attach existing albums for other members. The first reconciliation includes the union of their member-owned assets. Register only additional logical albums you want bridged. The catch-all album is registered automatically; existing member albums matching `together_album_name` are attached when unique, and missing ones are created on the first active cycle. Its name remains controlled by the config value. Other albums are not discovered automatically.

| Endpoint | Purpose |
| --- | --- |
| `GET /api/status` | Report `dry_run` or `active` mode |
| `GET /api/members` | Configured bridge IDs, Immich user IDs, and recipient library IDs |
| `GET /api/albums` | Registered logical albums and canonical metadata |
| `GET /api/replicas` | Asset IDs, paths, lifecycle states, and errors |
| `GET /api/assets/{logicalAssetId}` | One logical asset and its per-member replicas |
| `GET /api/filesystem` | Recipient file existence, inode equality, and link details |
| `POST /api/albums` | Register an Immich album and optional existing replicas |
| `PATCH /api/albums/{logicalAlbumId}` | Set `name`, `description`, and optional `coverLogicalAssetId` |
| `POST /api/reconcile` | Run one reconciliation cycle in active mode; HTTP 409 in dry-run mode |

The `PATCH` body should include the complete desired name and description; an omitted description becomes empty. Set a cover only after that logical asset is known and mapped. The bridge leaves the cover unchanged for a member until that member's asset ID is ready.

Expect `pending_import` for a while after a scan; later polls retry the lookup even if Immich's thumbnail and machine-learning jobs are still queued. The bridge prepares up to 500 new replicas, looks up up to 500 pending imports, and audits up to 200 ready replicas per member and album per cycle. It requests at most one recipient library scan per member every 90 seconds and adds album assets in batches of 100. Inspect `GET /api/replicas`, `GET /api/filesystem`, and the JSON container logs when an asset does not appear. A temporarily unavailable Immich API makes `/readyz` return 503. The bridge retains SQLite state and resumes on later cycles.

After a process crash or host reboot, the container's restart policy starts the bridge again. Its SQLite database uses WAL mode with full commit synchronization; incomplete transactions roll back. Reconciliation retries persisted `pending_link` and `pending_import` records, accepts a hardlink that already points to the correct source, and checks each imported asset's owner, library, and path before reusing it. Keep the SQLite state volume and originals available across restarts. If either is lost or damaged, recovery needs manual investigation and a backup.

## Current limits and production pilot

- Only ordinary single-file images and videos are modeled. Live Photos, motion photos, stacks, sidecars, edits, and other coupled forms are marked `unsupported`. Ordinary JPEGs have live end-to-end validation; videos still need a live trial.
- Removing an asset from one logical album removes only that album's sharing reference. After its final reference is gone, recipient files are marked `pending_removal` and are **not** unlinked; Immich assets are not deleted. Deleting an album produces a visible reconciliation error; a missing recipient asset may be rediscovered or left pending.
- The bridge does not create public shared links. It does not mirror favorites, people, edits, or other personal Immich metadata.
- If Immich moves a managed original to a storage-template path, the bridge accepts the new path only after matching the old source or existing recipient links by inode. A move that changes the inode needs manual investigation. Recipient paths remain stable.
- The service supports one family per SQLite database and at least two configured members. It has no workflow yet for removing members, changing their identities, or cleaning up abandoned recipient files.

For a real Immich instance, establish backups for SQLite state, the Immich database, and media; validate API-key scopes and all path mappings against that instance; and run a small album of disposable photos through the exact production mounts and service UID/GID. Confirm recipient inodes match their sources, imports resolve to the expected owner and library, and a bridge restart produces no new changes. Expand to important albums only after that pilot. The application does not modify or unlink Immich-managed originals, but the shared-file behavior of hardlinks and host permissions still matter.

## Development and releases

~~~sh
go test ./...
go vet ./...
goreleaser check
~~~

Unit tests use temporary media and a fake Immich client; there is no fake runtime mode. The GitHub workflows use Release Please on `master` and GoReleaser on a GitHub release. Releases publish only the multi-platform Docker image; compiled Go binaries are intermediate image inputs. See [CHANGELOG.md](CHANGELOG.md) for released changes.
