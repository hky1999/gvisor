"""Publish adjacent immutable native XFS snapshots into complete JuiceFS generations."""

import argparse
import bisect
import contextlib
import errno
import fcntl
import hashlib
import json
import os
from pathlib import Path
import struct
import subprocess
import time

from publish import compose, fingerprint, fsync_path, verify_images

FIEMAP = 0xC020660B
HEADER = struct.Struct("=QQIIII")
EXTENT = struct.Struct("=QQQQQIIII")
LAST, UNWRITTEN, SHARED = 1, 0x800, 0x2000
KNOWN = LAST | UNWRITTEN | SHARED
MAX_OFFSET = (1 << 64) - 1


def mappings(file):
    """Enumerate allocated ranges. Unknown flags force a file read for that range."""
    start = 0
    batch = 512
    while True:
        buf = bytearray(HEADER.size + batch * EXTENT.size)
        HEADER.pack_into(buf, 0, start, MAX_OFFSET - start, 1, 0, batch, 0)
        fcntl.ioctl(file.fileno(), FIEMAP, buf, True)
        count = HEADER.unpack_from(buf)[3]
        if not count:
            return
        for index in range(count):
            off, phys, length, _, _, flags, _, _, _ = EXTENT.unpack_from(
                buf, HEADER.size + index * EXTENT.size
            )
            if off < start or not length:
                raise ValueError("invalid FIEMAP extent")
            yield off, off + length, phys, flags
            start = off + length
        if flags & LAST:
            return


class LocalSnapshot:
    def __init__(self, directory, origin, stack):
        (entry,) = json.loads((directory / "filestores.json").read_text())["filestores"]
        if entry["file"] != "filestore-0":
            raise ValueError("expected one root filestore")
        self.entry = entry
        self.layer = (entry.get("delta") or {}).get("frozen")
        if not self.layer:
            raise ValueError("expected a frozen layer from --filestore-extent-snapshot")
        self.size = entry["size_bytes"]
        self.file = stack.enter_context(
            (directory / entry["file"]).open("rb", buffering=0)
        )
        st = os.fstat(self.file.fileno())
        if st.st_size != self.size:
            raise ValueError("truncated local snapshot")
        origin_stat = origin.stat()
        if (
            self.layer["base_inode"],
            self.layer["base_size"],
            self.layer["base_fingerprint"],
        ) != (origin_stat.st_ino, origin_stat.st_size, fingerprint(origin)):
            raise ValueError("snapshot does not belong to this immutable origin")
        self.identity = dict(
            id=self.layer["snapshot_id"],
            manifest=hashlib.sha256(
                json.dumps(entry, sort_keys=True).encode()
            ).hexdigest(),
            inode=st.st_ino,
            device=st.st_dev,
        )
        if not self.identity["id"]:
            raise ValueError("missing snapshot ID")
        self.local = []
        for r in self.layer["local_ranges"] or []:
            lo, end = r["offset"], r["offset"] + r["length"]
            if (
                not (0 <= lo < end <= self.size)
                or self.local
                and lo < self.local[-1][1]
            ):
                raise ValueError("invalid frozen ownership ranges")
            self.local.append((lo, end))
        self.map = []
        self.mapped = False
        if (
            subprocess.check_output(
                ["stat", "-f", "-L", "-c", "%t", f"/proc/self/fd/{self.file.fileno()}"],
                pass_fds=(self.file.fileno(),),
                text=True,
            ).strip()
            == "58465342"
        ):
            try:
                self.map = list(mappings(self.file))
                self.mapped = True
            except OSError as e:
                # EBADR is FIEMAP's response to an unsupported input flag.
                # A partial extent list also has to be discarded on fallback.
                if e.errno not in (errno.ENOTTY, errno.EOPNOTSUPP, errno.ENOSYS, errno.EBADR):
                    raise
        self.local_starts = [r[0] for r in self.local]
        self.map_starts = [r[0] for r in self.map]

    def boundaries(self):
        yield self.size
        yield self.layer["base_size"]
        for ranges in (self.local, self.map):
            for r in ranges:
                yield r[0]
                yield r[1]

    def at(self, pos):
        if pos >= self.size:
            return "zero", None
        i = bisect.bisect_right(self.local_starts, pos) - 1
        if pos < self.layer["base_size"] and (i < 0 or self.local[i][1] <= pos):
            return "origin", None
        if not self.mapped:
            return "data", None
        i = bisect.bisect_right(self.map_starts, pos) - 1
        if i < 0 or self.map[i][1] <= pos:
            return "zero", None
        lo, _, phys, flags = self.map[i]
        if flags & ~KNOWN:
            return "data", None
        if flags & UNWRITTEN:
            return "zero", None
        address = (
            (self.identity["device"], phys + pos - lo)
            if phys and flags & SHARED
            else None
        )
        return "data", address


def plan(origin_size, previous, current):
    bounds = {0, current.size, origin_size, *current.boundaries()}
    if previous:
        bounds.update(previous.boundaries())
    points = sorted(x for x in bounds if 0 <= x <= current.size)
    changes = []
    shared = 0
    for start, end in zip(points, points[1:]):
        old = (
            previous.at(start)
            if previous
            else ("origin" if start < origin_size else "zero", None)
        )
        new = current.at(start)
        if new == old and (new[0] != "data" or new[1] is not None):
            if new[0] == "data":
                shared += end - start
            continue
        kind = new[0]
        if (
            changes
            and changes[-1]["kind"] == kind
            and changes[-1]["offset"] + changes[-1]["length"] == start
        ):
            changes[-1]["length"] += end - start
        else:
            changes.append(dict(offset=start, length=end - start, kind=kind))
    return changes, shared


def capture_digests(snapshot, snapshot_id):
    capture = json.loads((snapshot / "capture.json").read_text())
    if capture["snapshot_id"] != snapshot_id:
        raise ValueError("wrong process image capture ID")
    images = capture["images"]
    if set(images) not in (
        {"checkpoint.img"},
        {"checkpoint.img", "pages_meta.img", "pages.img"},
    ):
        raise ValueError("unknown capture image set")
    return images


def completed_generation(output, receipt, image):
    if json.loads((output / "generation.json").read_text()) != receipt:
        raise ValueError("generation name already in use")
    verify_images(image, receipt["images"])
    fsync_path(output.parent)
    return {"alreadyPublished": True, "id": receipt["snapshot"]["id"]}


def publish(origin_dir, parent, previous_dir, snapshot, image, output, mount):
    origin = origin_dir / "filestores/filestore-0"
    parent_file = parent / "filestores/filestore-0"
    started = time.monotonic()
    with contextlib.ExitStack() as stack:
        current = LocalSnapshot(snapshot, origin, stack)
        previous = LocalSnapshot(previous_dir, origin, stack) if previous_dir else None
        if previous:
            receipt = json.loads((parent / "generation.json").read_text())
            if receipt["snapshot"] != previous.identity:
                raise ValueError(
                    "remote parent and previous local snapshot do not match"
                )
            if previous.identity["id"] == current.identity["id"]:
                raise ValueError("duplicate snapshot in adjacent pair")
        elif not parent.samefile(origin_dir):
            raise ValueError("missing previous local snapshot for non-origin parent")
        receipt = {
            "snapshot": current.identity,
            "parent": previous.identity["id"] if previous else None,
            "origin_inode": origin.stat().st_ino,
            "images": capture_digests(snapshot, current.identity["id"]),
        }
        if output.exists():
            return completed_generation(output, receipt, image)
        changes, shared = plan(origin.stat().st_size, previous, current)
        entry = dict(current.entry)
        (parent_entry,) = json.loads(
            (parent / "filestores/filestores.json").read_text()
        )["filestores"]
        entry["delta"] = dict(
            base_size=parent_entry["size_bytes"],
            base_fingerprint=parent_entry["fingerprint"],
            extents=[dict(r, zero=r["kind"] == "zero") for r in changes],
        )
        elapsed = time.monotonic() - started
        try:
            result = compose(
                parent_file,
                snapshot / entry["file"],
                entry,
                image,
                output,
                mount,
                origin=origin,
                receipt=receipt,
            )
        except OSError as err:
            # Concurrent publishers race only on the final directory rename.
            if err.errno not in (errno.EEXIST, errno.ENOTEMPTY) or not output.is_dir():
                raise
            return completed_generation(output, receipt, image)
        return dict(
            result,
            planSeconds=elapsed,
            sharedLocalBytes=shared,
            currentExtents=len(current.map),
            previousExtents=len(previous.map) if previous else 0,
            localRanges=len(current.local),
            fiemap=current.mapped,
            id=current.identity["id"],
        )


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("origin", "parent", "snapshot", "image", "output", "mount"):
        parser.add_argument("--" + name, type=Path, required=True)
    parser.add_argument("--previous", type=Path)
    a = parser.parse_args()
    print(
        json.dumps(
            publish(
                a.origin, a.parent, a.previous, a.snapshot, a.image, a.output, a.mount
            )
        )
    )
