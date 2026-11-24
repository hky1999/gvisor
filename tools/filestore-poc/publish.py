"""Publish a complete JuiceFS filestore from an immutable base and XFS changes."""

import argparse
import contextlib
import ctypes
import hashlib
import json
import os
from pathlib import Path
import shutil
import time
import tempfile

PAGE = 4096
COUNTERS = {
    "juicefs_object_request_data_bytes_PUT",
    "juicefs_object_request_data_bytes_GET",
    "juicefs_compact_size_histogram_bytes_total",
}


def stats(mount):
    # An untouched read-only mount may never instantiate the PUT series.
    result = dict.fromkeys(COUNTERS, 0)
    for line in (mount / ".stats").read_text().splitlines():
        parts = line.split()
        if len(parts) == 2 and parts[0] in COUNTERS:
            result[parts[0]] = int(float(parts[1]))
    return result


def diff(before, after):
    return {key: after[key] - before[key] for key in before}


def fingerprint(path):
    result = hashlib.sha256()
    size = path.stat().st_size
    with path.open("rb", buffering=0) as source:
        result.update(source.read(PAGE))
        if size > PAGE:
            source.seek(size - PAGE)
            result.update(source.read(PAGE))
    return result.hexdigest()


def range_copy(source, dest, length, source_offset, dest_offset):
    while length:
        copied = os.copy_file_range(source, dest, min(length, 1 << 30), source_offset, dest_offset)
        if copied <= 0:
            raise RuntimeError(f"short copy_file_range at offset {source_offset}")
        length -= copied
        source_offset += copied
        dest_offset += copied


def punch(fd, offset, length):
    libc = ctypes.CDLL(None, use_errno=True)
    libc.fallocate.argtypes = (ctypes.c_int, ctypes.c_int, ctypes.c_longlong, ctypes.c_longlong)
    if libc.fallocate(fd, 3, offset, length):  # PUNCH_HOLE | KEEP_SIZE
        error = ctypes.get_errno()
        raise OSError(error, os.strerror(error))


def fsync_path(path):
    fd = os.open(path, os.O_RDONLY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def verify_images(image, expected, destination=None):
    """Verify the bytes being published; reject missing and unexpected artifacts."""
    if {path.name for path in image.iterdir()} != set(expected):
        raise ValueError("process image file set differs from the capture")
    if destination is not None:
        destination.mkdir()
    total = 0
    for name, digest in expected.items():
        path = image / name
        if path.is_symlink() or not path.is_file():
            raise ValueError(f"invalid process image artifact {name}")
        hasher = hashlib.sha256()
        with contextlib.ExitStack() as stack:
            src = stack.enter_context(path.open("rb"))
            dst = stack.enter_context((destination / name).open("xb")) if destination is not None else None
            for block in iter(lambda: src.read(1 << 20), b""):
                hasher.update(block)
                total += len(block)
                if dst is not None:
                    dst.write(block)
            if hasher.hexdigest() != digest:
                raise ValueError(f"process image digest mismatch for {name}")
            if dst is not None:
                dst.flush()
                os.fsync(dst.fileno())
    return total


def compose(base, delta_file, entry, image, output, mount, *, origin=None, receipt=None):
    started = time.monotonic()
    delta = entry["delta"]
    size = entry["size_bytes"]
    if base.stat().st_size != delta["base_size"] or fingerprint(base) != delta["base_fingerprint"]:
        raise ValueError("wrong baseline for filestore delta")
    stages = {"before": stats(mount)}
    pending = Path(tempfile.mkdtemp(prefix="."+output.name+".pending-", dir=output.parent))
    try:
        filestores = pending / "filestores"
        filestores.mkdir()
        packed = pending / "delta.bin"
        target = filestores / entry["file"]
        extents = []
        previous_end = 0
        packed_length = 0
        zero_length = 0
        pack_start = time.monotonic()
        with delta_file.open("rb", buffering=0) as source, packed.open("xb") as dest:
            for extent in delta["extents"] or []:
                offset, length = extent["offset"], extent["length"]
                if not previous_end <= offset < offset + length <= size:
                    raise ValueError(f"bad filestore extent {extent}")
                previous_end = offset + length
                if extent.get("kind") == "origin":
                    extents.append((offset, length, "origin"))
                    continue
                if extent.get("zero"):
                    extents.append((offset, length, None))
                    zero_length += length
                    continue
                extents.append((offset, length, packed_length))
                source.seek(offset)
                remaining = length
                while remaining:
                    data = source.read(min(remaining, 4 << 20))
                    if not data:
                        raise OSError("short read while packing filestore delta")
                    dest.write(data)
                    remaining -= len(data)
                    packed_length += len(data)
            dest.flush()
            os.fsync(dest.fileno())
        pack_seconds = time.monotonic() - pack_start
        stages["packed"] = stats(mount)

        compose_start = time.monotonic()
        with base.open("rb", buffering=0) as source, target.open("xb", buffering=0) as dest:
            inherited_size = min(delta.get("base_limit", delta["base_size"]), size)
            range_copy(source.fileno(), dest.fileno(), inherited_size, 0, 0)
            dest.truncate(size)
            with packed.open("rb", buffering=0) as pack:
                for offset, length, pack_offset in extents:
                    if pack_offset == "origin":
                        with origin.open("rb", buffering=0) as original:
                            range_copy(original.fileno(), dest.fileno(), length, offset, offset)
                    elif pack_offset is None:
                        punch(dest.fileno(), offset, length)
                    else:
                        range_copy(pack.fileno(), dest.fileno(), length, pack_offset, offset)
            os.fsync(dest.fileno())
        stages["composed"] = stats(mount)
        compose_seconds = time.monotonic() - compose_start
        packed.unlink()

        sidecar = filestores / "filestores.json"
        sidecar.write_text(json.dumps({"filestores": [{k: v for k, v in entry.items() if k != "delta"}]}) + "\n")
        if receipt is not None:
            if fingerprint(target) != entry["fingerprint"]:
                raise ValueError("composed filestore differs from capture fingerprint")
            image_bytes = verify_images(image, receipt["images"], pending / "image")
        else:
            shutil.copytree(image, pending / "image")
            image_bytes = 0
            for path in (pending / "image").rglob("*"):
                if path.is_file():
                    image_bytes += path.stat().st_size
                    fsync_path(path)
        if receipt is not None:
            marker = pending / "generation.json"
            marker.write_text(json.dumps(receipt) + "\n")
            fsync_path(marker)
        for path in (pending / "image", sidecar, filestores, pending):
            fsync_path(path)
        pending.rename(output)
        fsync_path(output.parent)
        stages["published"] = stats(mount)
        return {
            "deltaBytes": packed_length,
            "zeroBytes": zero_length,
            "extents": len(extents),
            "stateBytes": image_bytes,
            "packSeconds": pack_seconds,
            "composeSeconds": compose_seconds,
            "publishSeconds": time.monotonic() - started,
            "packObjectCounters": diff(stages["before"], stages["packed"]),
            "composeObjectCounters": diff(stages["packed"], stages["composed"]),
            "totalObjectCounters": diff(stages["before"], stages["published"]),
        }
    finally:
        if pending.exists():
            shutil.rmtree(pending)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("base", "snapshot", "image", "output", "mount"):
        parser.add_argument(name, type=Path)
    args = parser.parse_args()
    entry, = json.loads((args.snapshot / "filestores.json").read_text())["filestores"]
    print(json.dumps(compose(args.base, args.snapshot / entry["file"], entry, args.image, args.output, args.mount)))
