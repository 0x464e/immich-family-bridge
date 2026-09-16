# Immich Family Bridge

Immich Family Bridge is a small reconciliation service for mirrored family albums and recipient-owned External Library assets. This first milestone operates **only with fake assets and a fake Immich backend**. It cannot connect the reconciler to a real Immich installation. The included Compose file mounts only `./testdata`.

## Run the safe fixture

```sh
go test ./...
docker compose up --build -d
curl http://127.0.0.1:8080/healthz
curl -H 'Authorization: Bearer local-fixture-token' \
  -H 'Content-Type: application/json' \
  -d '{"memberId":"alice","albumId":"alice-together"}' \
  http://127.0.0.1:8080/api/albums
curl -H 'Authorization: Bearer local-fixture-token' \
  -X POST http://127.0.0.1:8080/api/reconcile
curl -H 'Authorization: Bearer local-fixture-token' \
  http://127.0.0.1:8080/api/replicas
```

The bridge files appear under `testdata/bridge/families/demo-family/`. The SQLite database and fake backend state are in `testdata/state/`. They persist across container restarts. Delete those *fixture-only* files to reset the demo.

The default token is for localhost fixture use only. Set `FAMILYBRIDGE_API_TOKEN` when running anything other than this local demo. Health endpoints are public. The internal API requires `Authorization: Bearer <token>`:

- `GET /api/members`, `GET /api/albums`, `GET /api/replicas`, `GET /api/assets/{logicalAssetId}`, and `GET /api/filesystem` inspect mappings and link health.
- `POST /api/albums` registers an existing album. Include `memberId` and `albumId`; optionally include `replicas`, a map from other member IDs to their existing album IDs. The first reconciliation takes the union of those albums' assets.
- `PATCH /api/albums/{logicalAlbumId}` updates canonical `name`, `description`, and optional `coverLogicalAssetId`.
- `POST /api/reconcile` runs a cycle; `POST /api/reconcile/dry-run` reports proposed actions without changing state.

## Design and safety boundary

The bridge stores logical assets, one asset replica per member, logical albums, album replicas, membership references, and observations in SQLite. Registered albums are mirrored across all configured members. New member-owned assets added to any replica album become logical assets; mapped bridge assets are recognized as replicas so they cannot cause feedback loops. Album removals remove only that album's sharing reference. Unused recipient links are marked `pending_removal` and retained.

The linker accepts only regular source files under the configured fixture source root. It creates deterministic recipient paths, checks existing inode identity, rejects symlinks and path escapes, and reports `EXDEV` without copying. Fake mode requires sibling `source` and `bridge` directories and a fixture marker file. No API or workflow removes source files. Generated Immich media is outside the bridge design.

The HTTP Immich adapter implements a subset of the [Immich 3.2.0 OpenAPI schema pinned at commit `810454a`](https://github.com/immich-app/immich/blob/810454a34300e182fe1af04ea20cabc06198b08a/open-api/immich-openapi-specs.json), with a local schema fixture and contract tests. The running service deliberately rejects `mode: real`. Before enabling that mode, validate API-key permissions, exact path search, scan/import timing, External Library ownership, and source-to-container path mapping against a disposable Immich instance. Library scans require a separate admin API key in the HTTP adapter. Real Docker mounts must be tested for hardlink support: two bind mounts can return `EXDEV` even when their host paths are on one filesystem. Immich should mount recipient bridge files read-only; UID/GID and hardlink protection must be checked. Never mount production Immich media into this first milestone.

The initial reconciler skips coupled media forms, such as Live Photos, stacks, and edited assets. Immich-local metadata stays per user. Public links are reserved in the data model but are not created. The dry-run endpoint reports proposed actions without writing files, Immich data, or SQLite state.

## Releases

The repository uses the same release flow as `traefik-opnsense-sync`: conventional commits on `master` feed release-please, which maintains `CHANGELOG.md` and opens a release PR. Its first proposed version is `v0.1.0`. When a GitHub release is created, GoReleaser publishes only the `0x464e/immich-family-bridge` multi-platform Docker Hub image with version and `latest` tags. Linux amd64 and arm64 binaries are intermediate inputs to the image build; no standalone binaries or archives are attached to the GitHub release.

The GitHub repository must be created and connected later; this local checkout has no remote and nothing has been pushed. Before enabling the workflows on GitHub, configure `ACTIONS_PAT` for release-please and GoReleaser GitHub writes, and `DOCKER_PAT` for the `0x464e` Docker Hub account. The local Compose build continues to use `Dockerfile`; GoReleaser uses `Dockerfile.goreleaser` to package its prebuilt Linux binaries.

Local release validation without publishing:

```sh
goreleaser check
goreleaser release --snapshot --skip=archive,docker --clean
```
