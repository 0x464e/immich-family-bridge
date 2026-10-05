#!/usr/bin/env python3
"""Destructive ALBUM-ONLY lifecycle test on the guarded disposable localhost stack.

No asset/file DELETE calls are made. A nonexistent UUID is inserted into the
test recovery record to exercise missing-asset restoration without deleting media.
"""

import importlib.util
import json
import os
import secrets
import sqlite3
import subprocess
import sys
import time
import urllib.error
import uuid
from pathlib import Path

sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location("marker_live", Path(__file__).with_name("test-album-sharing.py"))
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)


def guard():
    server = json.loads(subprocess.check_output(["docker", "inspect", "immich_server"]))[0]
    m.check(any(x["Destination"] == "/data" and x["Source"] == str(m.MEDIA_ROOT / "immich-upload")
                for x in server["Mounts"]), "not disposable Immich")
    bridge = json.loads(subprocess.check_output(["docker", "inspect", "immich-family-bridge-familybridge-1"]))[0]
    m.check(any(x["Destination"] == "/state" and x["Source"] == str(m.TEST_ROOT / "bridge-state")
                for x in bridge["Mounts"]), "not disposable bridge")
    m.check(m.request("/api/status", bridge=True)["mode"] == "active", "bridge is not active")


def expect_error(path, code, method, bridge=True, key=None):
    try:
        m.request(path, method=method, bridge=bridge, key=key)
        raise AssertionError("unexpected success: " + path)
    except urllib.error.HTTPError as exc:
        m.check(exc.code == code, f"unexpected HTTP {exc.code}: {path}")


def main():
    guard()
    members = {name: m.KEYS[var] for name, var in [("admin", "IMMICH_MEMBER_ADMIN_KEY"),
               ("user1", "IMMICH_MEMBER_USER1_KEY"), ("user2", "IMMICH_MEMBER_USER2_KEY")]}
    for name, key in members.items():
        m.check(m.request("/users/me", key)["email"] == name + "@test.com", "unexpected identity")
    marker = next(u["id"] for u in m.request("/users", members["admin"]) if u["email"] == "together@test.com")
    dbpath = m.TEST_ROOT / "bridge-state/bridge.sqlite"
    c = sqlite3.connect(f"file:{dbpath}?mode=ro", uri=True)
    c.row_factory = sqlite3.Row
    together = c.execute("SELECT id FROM logical_albums WHERE system_key='together'").fetchone()[0]
    together_replicas = {r["member_id"]: r["immich_album_id"] for r in c.execute(
        "SELECT * FROM album_replicas WHERE logical_album_id=?", (together,))}
    stamp = secrets.token_hex(6)
    asset = m.upload(members["user1"])
    source = m.album(members["user1"], "Unmirror recovery " + stamp, [asset])
    logical = m.request("/api/albums", data={"memberId": "user1", "albumId": source}, bridge=True)["id"]
    m.reconcile()
    m.wait_complete(c, logical, 1, members, marker)
    source_assets = {name: {a["id"] for a in m.assets(key, together_replicas[name])}
                     for name, key in members.items()}
    old = {r["member_id"]: r["immich_album_id"] for r in c.execute(
        "SELECT * FROM album_replicas WHERE logical_album_id=?", (logical,))}
    files = list(c.execute("SELECT * FROM asset_replica_files WHERE logical_asset_id IN "
                          "(SELECT logical_asset_id FROM album_memberships WHERE logical_album_id=?)", (logical,)))
    inode_before = {p: os.stat(m.MEDIA_ROOT / p.removeprefix("/media/")).st_ino
                    for row in files for p in (row["source_path"], row["recipient_path"])}
    # Normal Share-dialog equivalent on a recipient, not the original account.
    m.request("/albums/" + old["user2"] + "/user/" + marker, members["user2"], method="DELETE")
    deadline = time.monotonic() + 90
    record = None
    while time.monotonic() < deadline:
        record = next((r for r in m.request("/api/albums/deleted", bridge=True) if r["album"]["id"] == logical), None)
        if record and record["state"] == "deleted":
            break
        time.sleep(2)
    m.check(record and record["state"] == "deleted", "scheduled UI deletion failed")
    m.check(record["requestedBy"] == "member:user2", "wrong deletion initiator")
    m.check(not any(a["id"] == logical for a in m.request("/api/albums", bridge=True)), "deleted album still active")
    for name, key in members.items():
        m.check(not any(a["id"] == old[name] for a in m.request("/albums?isOwned=true", key)),
                "unmirror did not delete every copy, including original")
        m.check(source_assets[name] == {a["id"] for a in m.assets(key, together_replicas[name])},
                "unmirror changed Together membership")
    for path, inode in inode_before.items():
        m.check(os.stat(m.MEDIA_ROOT / path.removeprefix("/media/")).st_ino == inode, "unmirror changed files")
    print("PASS: recipient UI unshare deleted all copies; Together and file inodes unchanged", flush=True)
    print(f"Recovery JSON size for one photo / three members: {len(json.dumps(record).encode())} bytes", flush=True)
    # Repeated delete and stale registration must not create a second recovery record.
    m.request("/api/albums/" + logical, method="DELETE", bridge=True)
    expect_error("/api/albums/unknown", 404, "DELETE")
    expect_error("/api/albums/unknown/restore", 404, "POST")
    try:
        m.request("/api/albums", data={"memberId": "user1", "albumId": source}, bridge=True)
        raise AssertionError("archived source re-registered")
    except urllib.error.HTTPError as exc:
        m.check(exc.code == 409, "unexpected stale registration response")
    # Restart the local dev bridge; snapshot and completion must survive it.
    subprocess.run(["docker", "restart", "immich-family-bridge-familybridge-1"], check=True, stdout=subprocess.DEVNULL)
    deadline = time.monotonic() + 40
    while time.monotonic() < deadline:
        try:
            if m.request("/api/status", bridge=True)["mode"] == "active":
                break
        except Exception:
            pass
        time.sleep(1)
    m.check(any(r["album"]["id"] == logical for r in m.request("/api/albums/deleted", bridge=True)),
            "restart lost recovery record")
    # Simulate an asset that disappeared while deleted; modify only this verified
    # disposable SQLite snapshot, never original/recipient media or Immich assets.
    missing = str(uuid.uuid4())
    record["replicas"][0]["assetIds"].append(missing)
    record["replicas"][0]["coverId"] = missing
    writable = sqlite3.connect(dbpath)
    writable.execute("UPDATE album_deletions SET record=? WHERE logical_album_id=?",
                     (json.dumps(record), logical))
    writable.commit()
    writable.close()
    m.request("/api/albums/" + logical + "/restore", method="POST", bridge=True)
    m.check(not any(r["album"]["id"] == logical for r in m.request("/api/albums/deleted", bridge=True)),
            "completed restore kept archive")
    m.wait_complete(c, logical, 1, members, marker)
    new = {r["member_id"]: r["immich_album_id"] for r in c.execute(
        "SELECT * FROM album_replicas WHERE logical_album_id=?", (logical,))}
    m.check(all(new[name] != old[name] for name in members), "restore reused deleted IDs")
    m.request("/api/albums/" + logical + "/restore", method="POST", bridge=True)
    logs = subprocess.check_output(["docker", "logs", "--since", "5m", "immich-family-bridge-familybridge-1"], stderr=subprocess.STDOUT).decode()
    m.check(missing in logs and "skipped missing asset" in logs, "missing ID was not logged")
    print("PASS: archive survived restart; restore skipped/logged missing ID, restored exact contents, removed record", flush=True)
    m.request("/api/albums/" + logical, method="DELETE", bridge=True)
    m.request("/api/albums/" + logical, method="DELETE", bridge=True)
    m.request("/api/albums/" + logical + "/restore", method="POST", bridge=True)
    m.wait_complete(c, logical, 1, members, marker)
    print("PASS: API delete and restore, repeated deletes/restores, and second lifecycle", flush=True)
    # Catch-all cannot be deleted via API, and a UI marker removal repairs it.
    expect_error("/api/albums/" + together, 409, "DELETE")
    expect_error("/api/albums/" + together + "/restore", 409, "POST")
    m.request("/albums/" + together_replicas["user2"] + "/user/" + marker, members["user2"], method="DELETE")
    m.reconcile()
    m.check(m.marker_present(members["user2"], together_replicas["user2"], marker), "Together share not repaired")
    for name, key in members.items():
        m.check(source_assets[name] == {a["id"] for a in m.assets(key, together_replicas[name])}, "Together contents changed")
    m.check(not any(r["album"]["id"] == together for r in m.request("/api/albums/deleted", bridge=True)), "Together archived")
    print("PASS: Together protected through API and UI; contents unchanged", flush=True)
    # Confirm album-only delete's scoped permission, without asset.delete.
    disposable = m.album(members["user1"], "Scoped album delete " + stamp, [])
    scoped = []
    try:
        for permitted in (False, True):
            permissions = ["album.read"] + (["album.delete"] if permitted else [])
            created = m.request("/api-keys", members["user1"], {"name": "album-delete-test", "permissions": permissions})
            scoped.append(created["apiKey"]["id"])
            if permitted:
                m.request("/albums/" + disposable, created["secret"], method="DELETE")
            else:
                expect_error("/albums/" + disposable, 403, "DELETE", bridge=False, key=created["secret"])
        print("PASS: album.delete works without asset.delete; missing permission returns 403", flush=True)
    finally:
        for kid in scoped:
            m.request("/api-keys/" + kid, members["user1"], method="DELETE")
    m.check(c.execute("PRAGMA quick_check").fetchone()[0] == "ok", "SQLite integrity failure")
    c.close()


if __name__ == "__main__":
    main()
