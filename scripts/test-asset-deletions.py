#!/usr/bin/env python3
"""Owner/recipient trash and permanent-deletion test on disposable localhost only.

Creates unique synthetic uploads. Verifies test accounts and Docker mounts before
writes; deletes only this run's assets. Leaves surviving fixtures for inspection.
"""
import importlib.util
import json
from pathlib import Path
import secrets
import sqlite3
import subprocess
import sys
import time
import urllib.error

sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location("helpers", Path(__file__).with_name("test-album-sharing.py"))
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)
CONTAINER = "immich-family-bridge-familybridge-1"


def wait(predicate, description, timeout=240):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            print("PASS: " + description, flush=True)
            return
        time.sleep(2)
    raise AssertionError("Timed out: " + description)


def guard():
    for container, destination, source in [
        ("immich_server", "/data", m.MEDIA_ROOT / "immich-upload"),
        (CONTAINER, "/state", m.TEST_ROOT / "bridge-state"),
        (CONTAINER, "/media", m.MEDIA_ROOT),
    ]:
        info = json.loads(subprocess.check_output(["docker", "inspect", container]))[0]
        m.check(any(x["Destination"] == destination and x["Source"] == str(source)
                    for x in info["Mounts"]), "unexpected container mount: " + container)
    members = {name: m.KEYS[var] for name, var in [
        ("admin", "IMMICH_MEMBER_ADMIN_KEY"), ("user1", "IMMICH_MEMBER_USER1_KEY"),
        ("user2", "IMMICH_MEMBER_USER2_KEY")]}
    for name, key in members.items():
        m.check(m.request("/users/me", key)["email"] == name + "@test.com", "unexpected account")
    m.check(m.request("/api/status", bridge=True)["mode"] == "active", "bridge is not active")
    return members


def control(action):
    subprocess.run(["docker", action, CONTAINER], check=True, stdout=subprocess.DEVNULL)
    if action != "stop":
        # /api/status has a stable response across bridge versions.
        def active():
            try:
                return m.request("/api/status", bridge=True)["mode"] == "active"
            except urllib.error.URLError:
                return False
        wait(active, "bridge active after " + action, timeout=90)


def local(path):
    m.check(path.startswith("/media/"), "unexpected local media path")
    return m.MEDIA_ROOT / path.removeprefix("/media/")


def main():
    members = guard()
    print("Immich version:", m.request("/server/version"), flush=True)
    marker = next(u["id"] for u in m.request("/users", members["admin"]) if u["email"] == "together@test.com")
    c = sqlite3.connect(f"file:{m.TEST_ROOT / 'bridge-state/bridge.sqlite'}?mode=ro", uri=True)
    c.row_factory = sqlite3.Row
    ids = [m.upload(members["user1"]) for _ in range(4)]
    # Associate XMP before sharing: one recipient-repair fixture and one owner-
    # permanent-deletion fixture exercise sidecars as well as the media path.
    sidecars = []
    for aid in [ids[1], ids[3]]:
        def settled():
            return m.request("/assets/" + aid, members["user1"])["originalPath"].startswith("/data/library/")
        wait(settled, "disposable upload reached storage-template path", timeout=90)
        asset = m.request("/assets/" + aid, members["user1"])
        source = m.MEDIA_ROOT / "immich-upload" / asset["originalPath"].removeprefix("/data/")
        sidecar = Path(str(source) + ".xmp")
        sidecar.write_text('<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#"><rdf:Description xmlns:xmp="http://ns.adobe.com/xap/1.0/" xmp:Rating="3"/></rdf:RDF></x:xmpmeta>')
        sidecars.append(sidecar)
    m.request("/jobs/sidecar", m.KEYS["IMMICH_LIBRARY_ADMIN_KEY"], {"command": "start", "force": False}, "PUT")
    for aid in [ids[1], ids[3]]:
        wait(lambda: any(f["type"] == "sidecar" for f in m.request("/asset-files?assetId=" + aid, members["user1"])),
             "source XMP associated by Immich")
    album = m.album(members["user1"], "Asset deletion " + secrets.token_hex(6), ids)
    logical = m.request("/api/albums", data={"memberId": "user1", "albumId": album}, bridge=True)["id"]
    m.reconcile()
    m.wait_complete(c, logical, 4, members, marker)
    rows = list(c.execute("SELECT * FROM asset_replicas WHERE logical_asset_id IN "
                          "(SELECT logical_asset_id FROM album_memberships WHERE logical_album_id=?)", (logical,)))
    lids = {r["immich_asset_id"]: r["logical_asset_id"] for r in rows if r["role"] == "origin"}
    recipients = {(r["logical_asset_id"], r["member_id"]): dict(r) for r in rows if r["role"] == "external_replica"}
    # Both recipient deletion types, with the bridge absent during user actions.
    control("stop")
    try:
        trash = recipients[lids[ids[0]], "user2"]
        permanent = recipients[lids[ids[1]], "user2"]
        m.request("/assets", members["user2"], {"ids": [trash["immich_asset_id"]], "force": False}, "DELETE")
        m.request("/assets", members["user2"], {"ids": [permanent["immich_asset_id"]], "force": True}, "DELETE")
        def permanently_gone():
            result = m.request("/search/metadata", members["user2"],
                               {"id": permanent["immich_asset_id"], "withDeleted": True, "size": 1})
            return not result["assets"]["items"]
        wait(permanently_gone, "recipient permanently deleted in Immich")
        m.check(m.request("/assets/" + trash["immich_asset_id"], members["user2"])["isTrashed"], "recipient did not enter trash")
    finally:
        control("start")
    wait(lambda: m.verify(c, logical, 4, members, marker), "both recipient deletions repaired with exact memberships and hardlinks")
    restored = c.execute("SELECT * FROM asset_replicas WHERE logical_asset_id=? AND member_id='user2'", (lids[ids[0]],)).fetchone()
    m.check(restored["immich_asset_id"] == trash["immich_asset_id"], "trash repair changed asset ID")
    m.check(not m.request("/assets/" + restored["immich_asset_id"], members["user2"])["isTrashed"], "recipient still trashed")
    replacement = c.execute("SELECT * FROM asset_replicas WHERE logical_asset_id=? AND member_id='user2'", (lids[ids[1]],)).fetchone()
    m.check(replacement["immich_asset_id"] != permanent["immich_asset_id"], "permanent deletion not reimported")
    for aid in ids:
        m.check(not m.request("/assets/" + aid, members["user1"])["isTrashed"], "recipient deletion affected owner")
    print("PASS: recipient trash restored same ID; permanent deletion reimported; owner unchanged", flush=True)

    for aid in [ids[1], ids[3]]:
        def sidecars_ready():
            copies = list(c.execute("SELECT * FROM asset_replicas WHERE logical_asset_id=?", (lids[aid],)))
            return all(any(f["type"] == "sidecar" for f in m.request("/asset-files?assetId=" + r["immich_asset_id"], members[r["member_id"]])) for r in copies)
        wait(sidecars_ready, "recipient XMP associations present after repair")
    # Owner deletion must remove every recipient, even after the original's path
    # has already disappeared before the bridge can inspect it.
    doomed = {lids[ids[2]], lids[ids[3]]}
    files = list(c.execute("SELECT * FROM asset_replica_files WHERE logical_asset_id IN (?,?)", tuple(doomed)))
    paths = [local(r["recipient_path"]) for r in files if r["source_path"] != r["recipient_path"]]
    original = c.execute("SELECT filesystem_path FROM asset_replicas WHERE logical_asset_id=? AND role='origin'", (lids[ids[3]],)).fetchone()[0]
    control("stop")
    try:
        m.request("/assets", members["user1"], {"ids": [ids[2]], "force": False}, "DELETE")
        m.request("/assets", members["user1"], {"ids": [ids[3]], "force": True}, "DELETE")
        wait(lambda: not local(original).exists() and not sidecars[1].exists(), "permanently deleted owner's original file gone before bridge restart")
    finally:
        control("start")
    def decision_persisted():
        return all(c.execute("SELECT state FROM asset_replicas WHERE logical_asset_id=? AND role='origin'", (lid,)).fetchone()[0] == "owner_deleted" for lid in doomed)
    wait(decision_persisted, "owner deletion decision persisted in SQLite")
    control("restart")
    def removed():
        if any(c.execute("SELECT 1 FROM sharing_sources WHERE logical_asset_id=?", (lid,)).fetchone() for lid in doomed):
            return False
        if any(c.execute("SELECT 1 FROM asset_replicas WHERE logical_asset_id=? AND role='external_replica'", (lid,)).fetchone() for lid in doomed):
            return False
        return not any(p.exists() for p in paths)
    wait(removed, "owner trash and permanent deletion removed every recipient asset, mapping, and hardlink")
    wait(lambda: m.verify(c, logical, 2, members, marker), "surviving assets and album memberships unchanged")
    m.check(m.request("/assets/" + ids[2], members["user1"])["isTrashed"], "bridge restored owner's trash")
    control("restart")
    m.reconcile()
    m.check(removed(), "restart resurrected owner-deleted asset")
    m.check(c.execute("PRAGMA integrity_check").fetchone()[0] == "ok", "SQLite integrity check failed")
    print("PASS: restart did not resurrect owner deletions; SQLite integrity ok", flush=True)
    print("Fixture logical album:", logical, flush=True)


if __name__ == "__main__":
    main()
