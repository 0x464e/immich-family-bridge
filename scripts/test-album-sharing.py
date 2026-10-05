#!/usr/bin/env python3
"""Live test for the local disposable Immich setup; never accepts remote URLs.

Start /home/otto/immich-test/docker-compose.yml and the dev bridge configured
with together_user_id before running this script. Fixtures are left available
for inspection. Temporary scoped API keys are revoked before exit.
"""

import datetime
import json
import os
from pathlib import Path
import secrets
import sqlite3
import struct
import subprocess
import time
import urllib.error
import urllib.request
import zlib


TEST_ROOT = Path("/home/otto/immich-test")
MEDIA_ROOT = Path("/mnt/media/immich-family-bridge-test")
IMMICH = "http://127.0.0.1:2283/api"
BRIDGE = "http://127.0.0.1:8081"
KEYS = {}
for line in (TEST_ROOT / ".bridge-keys.env").read_text().splitlines():
    if line and not line.startswith("#") and "=" in line:
        name, value = line.split("=", 1)
        KEYS[name] = value.strip().strip("\"'")


def request(path, key=None, data=None, method=None, bridge=False, raw=None, content=None):
    headers = {"Content-Type": content or "application/json"}
    if bridge:
        headers["Authorization"] = "Bearer " + KEYS["FAMILYBRIDGE_API_TOKEN"]
    elif key:
        headers["x-api-key"] = key
    payload = raw if raw is not None else json.dumps(data).encode() if data is not None else None
    req = urllib.request.Request((BRIDGE if bridge else IMMICH) + path,
                                 headers=headers, data=payload, method=method)
    with urllib.request.urlopen(req, timeout=45) as response:
        body = response.read()
        return json.loads(body) if body else None


def check(condition, message):
    if not condition:
        raise AssertionError(message)


def chunk(kind, data):
    return struct.pack(">I", len(data)) + kind + data + struct.pack(">I", zlib.crc32(kind + data))


def upload(key):
    # Unique, synthetic media generated in memory, with no external downloads.
    pixels = b"".join(b"\x00" + secrets.token_bytes(64 * 3) for _ in range(64))
    png = (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", 64, 64, 8, 2, 0, 0, 0))
           + chunk(b"IDAT", zlib.compress(pixels)) + chunk(b"IEND", b""))
    boundary = "familybridge-" + secrets.token_hex(12)
    stamp = datetime.datetime.now(datetime.timezone.utc).isoformat().replace("+00:00", "Z")
    body = b""
    for field in ("fileCreatedAt", "fileModifiedAt"):
        body += (f'--{boundary}\r\nContent-Disposition: form-data; name="{field}"\r\n\r\n{stamp}\r\n').encode()
    body += (f'--{boundary}\r\nContent-Disposition: form-data; name="assetData"; '
             f'filename="marker-{secrets.token_hex(8)}.png"\r\nContent-Type: image/png\r\n\r\n').encode()
    body += png + f"\r\n--{boundary}--\r\n".encode()
    return request("/assets", key, method="POST", raw=body,
                   content="multipart/form-data; boundary=" + boundary)["id"]


def album(key, name, ids):
    result = request("/albums", key, {"albumName": name, "description": "Disposable marker feature test"})
    request("/albums/" + result["id"] + "/assets", key, {"ids": ids}, "PUT")
    return result["id"]


def share(key, aid, uid):
    request("/albums/" + aid + "/users", key,
            {"albumUsers": [{"userId": uid, "role": "viewer"}]}, "PUT")


def assets(key, aid):
    filt = {"albumIds": [aid], "size": 1000, "withStacked": True, "page": 1}
    result = []
    while True:
        page = request("/search/metadata", key, filt)["assets"]
        result.extend(page["items"])
        if page.get("nextCursor"):
            filt.pop("page", None)
            filt["cursor"] = page["nextCursor"]
        elif page.get("nextPage"):
            filt["page"] = int(page["nextPage"])
        else:
            return result


def reconcile():
    check(request("/api/reconcile", method="POST", bridge=True)["status"] == "ok", "reconcile failed")


def marker_present(key, aid, marker):
    users = request("/albums/" + aid, key)["albumUsers"]
    return any(u["user"]["id"] == marker and u["role"] == "viewer" for u in users)


def verify(c, logical, expected, members, marker):
    rows = list(c.execute("SELECT ar.* FROM asset_replicas ar JOIN album_memberships am "
                          "ON am.logical_asset_id=ar.logical_asset_id WHERE am.logical_album_id=?", (logical,)))
    ready = sum(r["state"] == "ready" for r in rows)
    if len(rows) != expected * len(members) or ready != len(rows):
        return False
    for member, key in members.items():
        aid = c.execute("SELECT immich_album_id FROM album_replicas WHERE logical_album_id=? AND member_id=?",
                        (logical, member)).fetchone()[0]
        want = {r["immich_asset_id"] for r in rows if r["member_id"] == member}
        actual = {a["id"] for a in assets(key, aid)}
        if actual != want:
            return False
        check(marker_present(key, aid, marker), "replica is not shared with marker")
        together = c.execute("SELECT immich_album_id FROM album_replicas ar JOIN logical_albums la "
                             "ON la.id=ar.logical_album_id WHERE la.system_key=? AND ar.member_id=?",
                             ("together", member)).fetchone()[0]
        check(want <= {a["id"] for a in assets(key, together)}, "assets missing from Together")
    files = list(c.execute("SELECT rf.* FROM asset_replica_files rf JOIN album_memberships am "
                           "ON am.logical_asset_id=rf.logical_asset_id WHERE am.logical_album_id=? "
                           "AND rf.source_path<>rf.recipient_path", (logical,)))
    for item in files:
        paths = []
        for field in ("source_path", "recipient_path"):
            check(item[field].startswith("/media/"), "unexpected media path")
            paths.append(MEDIA_ROOT / item[field].removeprefix("/media/"))
        check(os.path.samefile(*paths), "recipient file differs from source inode")
    check(not any(r["error"] for r in rows), "replica errors present")
    return True


def wait_complete(c, logical, count, members, marker):
    deadline = time.monotonic() + 240
    while time.monotonic() < deadline:
        if verify(c, logical, count, members, marker):
            print(f"PASS: {logical}: {count} assets match across all accounts and hardlinks", flush=True)
            return
        time.sleep(3)
    raise AssertionError("imports/memberships did not finish within four minutes")


def main():
    # Validate identities and mounts before making any writes.
    inspect = json.loads(subprocess.check_output(["docker", "inspect", "immich_server"]))[0]
    mount = next(m for m in inspect["Mounts"] if m["Destination"] == "/data")
    check(mount["Source"] == str(MEDIA_ROOT / "immich-upload"), "not the disposable Immich mount")
    bridge = json.loads(subprocess.check_output(["docker", "inspect", "immich-family-bridge-familybridge-1"]))[0]
    check(any(m["Destination"] == "/state" and m["Source"] == str(TEST_ROOT / "bridge-state")
              for m in bridge["Mounts"]), "not the disposable bridge state")
    members = {"admin": KEYS["IMMICH_MEMBER_ADMIN_KEY"], "user1": KEYS["IMMICH_MEMBER_USER1_KEY"],
               "user2": KEYS["IMMICH_MEMBER_USER2_KEY"]}
    user_ids = {}
    for member, key in members.items():
        me = request("/users/me", key)
        check(me["email"] == member + "@test.com", "unexpected user identity")
        user_ids[member] = me["id"]
    users = request("/users", members["admin"])
    marker = next(u["id"] for u in users if u["email"] == "together@test.com")
    check(request("/api/status", bridge=True)["mode"] == "active", "bridge is not active")
    c = sqlite3.connect(f"file:{TEST_ROOT / 'bridge-state/bridge.sqlite'}?mode=ro", uri=True)
    c.row_factory = sqlite3.Row
    reconcile()
    for row in c.execute("SELECT * FROM album_replicas"):
        check(marker_present(members[row["member_id"]], row["immich_album_id"], marker),
              "legacy registration was not backfilled")
    print("PASS: existing registrations, including Together, backfilled as viewer", flush=True)

    stamp = str(int(time.time()))
    source1, source2 = upload(members["user1"]), upload(members["user2"])
    auto1 = album(members["user1"], "Marker live " + stamp, [source1])
    auto2 = album(members["user2"], "Marker live " + stamp, [source2])
    ordinary = album(members["user1"], "Ordinary sharing " + stamp, [source1])
    share(members["user1"], ordinary, user_ids["user2"])
    scoped = []
    try:
        # Exercise the exact permission required by the HTTP sharing adapter.
        for allowed in (False, True):
            perms = ["user.read", "album.read"] + (["albumUser.create"] if allowed else [])
            created = request("/api-keys", members["user1"], {"name": "marker-permission-test", "permissions": perms})
            scoped.append(created["apiKey"]["id"])
            try:
                share(created["secret"], auto1, marker)
                check(allowed, "sharing succeeded without albumUser.create")
            except urllib.error.HTTPError as exc:
                check(not allowed and exc.code == 403, f"unexpected scoped permission error: {exc.code}")
        print("PASS: albumUser.create permits sharing; restricted key gets HTTP 403", flush=True)
        share(members["user2"], auto2, marker)
        # Let automatic discovery pick these up; do not manually register them.
        deadline = time.monotonic() + 90
        while time.monotonic() < deadline:
            mappings = [c.execute("SELECT logical_album_id FROM album_replicas WHERE immich_album_id=?", (a,)).fetchone()
                        for a in (auto1, auto2)]
            if all(mappings):
                break
            time.sleep(3)
        check(all(mappings), "automatic discovery did not register marked albums")
        check(mappings[0][0] != mappings[1][0], "same-name albums were incorrectly merged")
        check(c.execute("SELECT 1 FROM album_replicas WHERE immich_album_id=?", (ordinary,)).fetchone() is None,
              "ordinary shared album was registered")
        print("PASS: scheduled discovery registered two marked albums; ordinary sharing ignored", flush=True)
        for logical in mappings:
            wait_complete(c, logical[0], 1, members, marker)

        api_album = album(members["user1"], "API registration " + stamp, [source1])
        body = {"memberId": "user1", "albumId": api_album}
        logical = request("/api/albums", data=body, bridge=True)["id"]
        for row in c.execute("SELECT * FROM album_replicas WHERE logical_album_id=?", (logical,)):
            check(marker_present(members[row["member_id"]], row["immich_album_id"], marker),
                  "API registration did not immediately mark replica")
        check(request("/api/albums", data=body, bridge=True)["id"] == logical, "repeat registration duplicated album")
        reconcile()
        wait_complete(c, logical, 1, members, marker)
        print("PASS: API registration marks every replica immediately and is idempotent", flush=True)

        extra = upload(members["user1"])
        request("/albums/" + auto1 + "/assets", members["user1"], {"ids": [extra]}, "PUT")
        reconcile()
        wait_complete(c, mappings[0][0], 2, members, marker)
        request("/albums/" + auto1 + "/user/" + marker, members["user1"], method="DELETE")
        reconcile()
        archived = request("/api/albums/deleted", bridge=True)
        check(any(r["album"]["id"] == mappings[0][0] and r["state"] == "deleted" for r in archived),
              "removed marker did not archive/delete the logical album")
        check(not any(a["id"] == auto1 for a in request("/albums?isOwned=true", members["user1"])),
              "unmirror retained original album")
        request("/api/albums/" + mappings[0][0] + "/restore", method="POST", bridge=True)
        wait_complete(c, mappings[0][0], 2, members, marker)
        total = c.execute("SELECT COUNT(*) FROM logical_albums").fetchone()[0]
        reconcile()
        check(c.execute("SELECT COUNT(*) FROM logical_albums").fetchone()[0] == total,
              "discovery registered existing replicas again")
        print("PASS: later additions sync; marker removal deletes albums; restore retains media without duplication", flush=True)
    finally:
        for key_id in scoped:
            request("/api-keys/" + key_id, members["user1"], method="DELETE")
        c.close()


if __name__ == "__main__":
    main()
