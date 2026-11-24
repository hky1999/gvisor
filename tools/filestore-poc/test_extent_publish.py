"""Run on a reflink-capable XFS with TMPDIR on that filesystem."""

from concurrent.futures import ThreadPoolExecutor
import contextlib
import errno
import threading
from unittest import mock
import fcntl
import hashlib
import json
import mmap
import os
from pathlib import Path
import random
import tempfile
import unittest

from extent_publish import LocalSnapshot, plan, publish
from publish import fingerprint, punch

PAGE = 4096
FICLONE = 0x40049409


class Generations(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.origin = self.root / "origin"
        self.origin.write_bytes(bytes([71]) * PAGE * 64)
        self.live = (self.root / "live").open("w+b", buffering=0)
        self.addCleanup(self.live.close)
        self.live.truncate(self.origin.stat().st_size)
        self.owned = set()
        self.stack = contextlib.ExitStack()
        self.addCleanup(self.stack.close)

    def capture(self, generation):
        directory = self.root / str(generation)
        directory.mkdir()
        with (directory / "filestore-0").open("xb") as clone:
            fcntl.ioctl(clone, FICLONE, self.live.fileno())
            os.fsync(clone.fileno())
        size = os.fstat(self.live.fileno()).st_size
        ranges = [
            dict(offset=i * PAGE, length=PAGE)
            for i in sorted(self.owned)
            if i * PAGE < size
        ]
        layer = dict(
            base_inode=self.origin.stat().st_ino,
            base_size=self.origin.stat().st_size,
            base_fingerprint=fingerprint(self.origin),
            snapshot_id=str(generation),
            local_ranges=ranges,
        )
        entry = dict(file="filestore-0", size_bytes=size, delta=dict(frozen=layer))
        (directory / "filestores.json").write_text(json.dumps(dict(filestores=[entry])))
        return LocalSnapshot(directory, self.origin, self.stack)

    def publication(self):
        origin = self.root / "g0"
        stores = origin / "filestores"
        stores.mkdir(parents=True)
        os.link(self.origin, stores / "filestore-0")
        entry = dict(
            file="filestore-0",
            size_bytes=self.origin.stat().st_size,
            fingerprint=fingerprint(self.origin),
        )
        (stores / "filestores.json").write_text(json.dumps(dict(filestores=[entry])))
        (self.root / ".stats").touch()
        images = self.root / "i1"
        images.mkdir()
        for name in ("checkpoint.img", "pages.img", "pages_meta.img"):
            (images / name).write_bytes(os.urandom(PAGE))
        self.owned.add(3)
        os.pwrite(self.live.fileno(), b"B" * PAGE, 3 * PAGE)
        self.capture(1)
        first = self.root / "1"
        manifest = first / "filestores.json"
        saved = json.loads(manifest.read_text())
        saved["filestores"][0]["fingerprint"] = entry["fingerprint"]
        manifest.write_text(json.dumps(saved))
        hashes = {
            p.name: hashlib.sha256(p.read_bytes()).hexdigest() for p in images.iterdir()
        }
        (first / "capture.json").write_text(
            json.dumps(dict(snapshot_id="1", images=hashes))
        )
        return origin, origin, None, first, images, self.root / "g1", self.root

    def test_publish_and_retry(self):
        args = self.publication()
        origin, _, _, snap, images, output, mount = args
        self.assertEqual(publish(*args)["deltaBytes"], PAGE)
        self.assertTrue(publish(*args)["alreadyPublished"])
        expected = bytearray(self.origin.read_bytes())
        expected[3 * PAGE : 4 * PAGE] = b"B" * PAGE
        self.assertEqual((output / "filestores/filestore-0").read_bytes(), expected)
        self.assertEqual(
            (output / "image/pages.img").read_bytes(),
            (images / "pages.img").read_bytes(),
        )
        self.capture(2)
        binding = json.loads((snap / "capture.json").read_text())
        binding["snapshot_id"] = "2"
        (self.root / "2/capture.json").write_text(json.dumps(binding))
        with self.assertRaisesRegex(ValueError, "already in use"):
            publish(origin, origin, None, self.root / "2", images, output, mount)

    def test_mismatched_memory_artifacts(self):
        args = self.publication()
        image, output = args[4:6]
        for name in ("checkpoint.img", "pages_meta.img", "pages.img"):
            file = image / name
            original = file.read_bytes()
            file.write_bytes(os.urandom(len(original)))
            with self.assertRaisesRegex(ValueError, "digest mismatch"):
                publish(*args)
            self.assertFalse(output.exists())
            file.unlink()
            with self.assertRaisesRegex(ValueError, "file set"):
                publish(*args)
            file.write_bytes(original)
        (image / "extra").touch()
        with self.assertRaisesRegex(ValueError, "file set"):
            publish(*args)
        self.assertEqual(list(self.root.glob(".g1.pending-*")), [])
        binding = args[3] / "capture.json"
        bad = json.loads(binding.read_text())
        bad["snapshot_id"] = "unrelated"
        binding.write_text(json.dumps(bad))
        with self.assertRaisesRegex(ValueError, "capture ID"):
            publish(*args)

    def test_concurrent_publish(self):
        import publish as publisher

        args = self.publication()
        barrier = threading.Barrier(2)
        original = publisher.fingerprint

        def meet(path):
            if path.name == "filestore-0" and ".pending-" in str(path):
                barrier.wait(timeout=5)
            return original(path)

        with (
            mock.patch.object(publisher, "fingerprint", side_effect=meet),
            ThreadPoolExecutor(2) as pool,
        ):
            results = list(pool.map(lambda _: publish(*args), range(2)))
        self.assertEqual(sum(r.get("alreadyPublished", False) for r in results), 1)
        self.assertEqual(list(self.root.glob(".g1.pending-*")), [])

    def test_enospc_and_post_rename_retry(self):
        args = self.publication()
        with mock.patch(
            "publish.range_copy", side_effect=OSError(errno.ENOSPC, "full")
        ):
            with self.assertRaises(OSError):
                publish(*args)
        self.assertFalse(args[5].exists())
        from publish import fsync_path

        def fail_after_rename(path):
            if path == args[5].parent and args[5].exists():
                raise OSError(errno.EIO, "commit acknowledgement lost")
            fsync_path(path)

        with mock.patch("publish.fsync_path", side_effect=fail_after_rename):
            with self.assertRaises(OSError):
                publish(*args)
        self.assertTrue(publish(*args)["alreadyPublished"])

    def test_sparse_fiemap_pagination(self):
        size = 1400 * PAGE
        self.live.truncate(size)
        for n in range(0, 1400, 2):
            os.pwrite(self.live.fileno(), b"A" * PAGE, n * PAGE)
            self.owned.add(n)
        first = self.capture(1)
        self.assertGreater(len(first.map), 512)
        os.pwrite(self.live.fileno(), b"B" * PAGE, 1398 * PAGE)
        second = self.capture(2)
        changes, _ = plan(self.origin.stat().st_size, first, second)
        self.assertEqual(changes, [dict(kind="data", offset=1398 * PAGE, length=PAGE)])

    def test_randomized_oracle(self):
        rng = random.Random(311)
        base = self.origin.read_bytes()
        previous = None
        parent = base
        for generation in range(12):
            pages = rng.randrange(48, 81)
            self.live.truncate(pages * PAGE)
            self.owned = {i for i in self.owned if i < pages}
            for _ in range(15):
                i = rng.randrange(pages)
                action = rng.randrange(4)
                if action == 0:
                    self.owned.discard(i)  # re-inherit even if stale local data exists
                elif action == 1:
                    self.owned.add(i)
                    punch(self.live.fileno(), i * PAGE, PAGE)
                else:
                    self.owned.add(i)
                    os.pwrite(
                        self.live.fileno(), bytes([rng.randrange(256)]) * PAGE, i * PAGE
                    )
            current = self.capture(generation)
            self.assertTrue(current.mapped)
            for fallback in (False, True):
                current.mapped = not fallback
                changes, _ = plan(len(base), previous, current)
                output = bytearray(parent[: current.size].ljust(current.size, b"\0"))
                for change in changes:
                    off, n = change["offset"], change["length"]
                    output[off : off + n] = {
                        "zero": lambda: bytes(n),
                        "origin": lambda: base[off : off + n],
                        "data": lambda: os.pread(current.file.fileno(), n, off),
                    }[change["kind"]]()
                truth = b"".join(
                    os.pread(self.live.fileno(), PAGE, i * PAGE)
                    if i in self.owned or i >= 64
                    else base[i * PAGE : (i + 1) * PAGE]
                    for i in range(pages)
                )
                self.assertEqual(
                    hashlib.sha256(output).digest(), hashlib.sha256(truth).digest()
                )
            current.mapped = True
            previous = current
            parent = truth

    def test_persistent_mmap_and_sparse_semantics(self):
        for i in (0, 1, 2):
            os.pwrite(self.live.fileno(), b"A" * PAGE, i * PAGE)
            self.owned.add(i)
        with mmap.mmap(self.live.fileno(), 4 * PAGE) as mapping:
            mapping[0] = 65
            first = self.capture(0)
            mapping[PAGE] = 88
            os.pwrite(self.live.fileno(), b"A" * PAGE, 2 * PAGE)
            self.owned.add(3)  # the physical hole now hides nonzero origin data
            second = self.capture(1)
            changes, shared = plan(self.origin.stat().st_size, first, second)
            self.assertGreaterEqual(shared, PAGE)
            self.assertEqual(
                changes,
                [
                    dict(offset=PAGE, length=2 * PAGE, kind="data"),
                    dict(offset=3 * PAGE, length=PAGE, kind="zero"),
                ],
            )
            self.assertEqual(os.pread(first.file.fileno(), 1, PAGE), b"A")
            self.assertEqual(os.pread(second.file.fileno(), 1, PAGE), b"X")


if __name__ == "__main__":
    unittest.main()
