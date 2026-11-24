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
