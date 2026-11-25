# Native layered filestore PoC

Use a state checkpoint saved with `--skip-filestore-pages` and its complete
filestore directory on a read-only JuiceFS client. Run the Systrap sandbox
with `--filestore-lazy-restore`, `--filestore-adopt-dir=BASE_DIR`, and a root
`--overlay2=root:dir=LOCAL_XFS`. Keep the base generation available for the
lifetime of the sandbox.

For tmpfs regular files the Sentry translates inherited ranges to the read-only
baseline FD. A shared writable VMA starts with a read-only translation for an
inherited page. On the first store the Sentry revokes old aliases in all
mapping spaces, copies the page to the sparse local file, then retries the
fault. A full aligned page supplied by pwrite can be installed locally
without fetching its previous contents. Partial overwrites preserve the rest
of the page. Private mappings retain guest private CoW semantics. Long-term
shared pins can materialize pages early so that their backing stays stable.
The base mapping reserves virtual address space for the logical file size.
The host accounts resident pages in the page cache as they are accessed.

To reproduce the alias check, build the guest probe with
`cc -O2 -static workload.c -o workload` and include it in the guest image.
Run `/workload seed` before saving G0. On B run `/workload aliases`.
The parent faults two inherited pages through an O_RDONLY, MAP_SHARED mapping.
A child changes one byte by a shared store and replaces the other page with
pwrite. After waitpid the parent checks every byte through the original
mapping. After saving G1 and restoring C, run `/workload aliases-verify`.
The probe exits nonzero on any mismatch.

To checkpoint the restored sandbox, use `--skip-filestore-pages` and
`--filestore-snapshot-dir=SNAPSHOT_DIR`. Put the snapshot directory on the
same reflink-capable XFS mount as the live filestore. Use
`--filestore-parent-mountns=true` on hosts that require the source and
destination to share the same mount namespace for reflink.

The local snapshot contains the materialized data and an extent index in
`filestores.json`. The index describes the data and holes needed to compose
the next complete filestore. Supply the baseline and output paths on the same
writable JuiceFS mount when running the publisher.

```sh
python3 tools/filestore-poc/publish.py BASE_FILE SNAPSHOT_DIR IMAGE_DIR OUTPUT_DIR JUICEFS_MOUNT
```

The publisher uploads the modified ranges, composes the full file with
`copy_file_range`, applies holes, and publishes the image and its filestore
together. A new sandbox uses the complete published filestore as its base.

### Successive checkpoints on one running sandbox

Keep the restored sandbox on its original read-only JuiceFS baseline and its
live XFS inode. Capture a unique image directory and snapshot directory per
generation, in the same checkpoint call:

```sh
runsc checkpoint --leave-running --skip-filestore-pages --filestore-extent-snapshot \
  --filestore-snapshot-dir="$local/s2" --image-path="$local/i2" "$container"

python3 tools/filestore-poc/extent_publish.py \
  --origin "$shared/g0" --parent "$shared/g1" --previous "$local/s1" \
  --snapshot "$local/s2" --image "$local/i2" --output "$shared/g2" --mount "$shared_mount"
```

For generation one, pass the origin as the parent and omit `--previous`.
The snapshot flag records a per-capture ID and sorted ranges owned by XFS.
After the save RPC, runsc hashes the completed local process image and binds the
digests to that ID in capture.json before publishing filestores.json. Publication
verifies the exact file set and hashes the bytes copied to the staging directory.
Uncompressed images include checkpoint.img, pages_meta.img, and pages.img.
Compressed images put their state in checkpoint.img. Older unbound local captures
need the matching earlier publisher; existing complete generations remain
readable with their compatible runtime.
A local hole in an owned range reads as zero. An unowned range inside the
baseline inherits it. Each generation's process image and memory pages come
from its matching image directory. Only the private filestore uses range-based
incremental publication. Use immutable, uniquely named directories and one
ordered publishing task per lineage. Keep the parent generation and its local
snapshot until its successor commits. A retry of an already committed
generation verifies the receipt. Restore directly from the chosen generation.

The experimental publisher supports one filestore per sandbox and a stable
JuiceFS volume. Keep both frozen files on a managed XFS mount without concurrent
extent relocation, deduplication or defragmentation. The fast path compares
synced, known written shared extents by logical offset, device and physical
address. It replays uncertain ranges conservatively. Snapshot identity binds
the published parent to its retained local manifest and inode. Use the matching
image directory from the same capture. The filestore fingerprint samples the
file ends. Shared-range reuse relies on the immutability of the published
parent and the stability of the frozen XFS extents throughout planning.
Process image files are verified against their capture SHA-256 digests.
After a publisher process crash an
uncommitted private staging directory can be reclaimed once no writer holds it.

Run the disk-level oracle on a scratch XFS filesystem:

```sh
TMPDIR=/path/to/writable/xfs python3 tools/filestore-poc/test_extent_publish.py -v
```
