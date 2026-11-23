# Native layered filestore PoC

Use a state checkpoint saved with `--skip-filestore-pages` and its complete
filestore directory on a read-only JuiceFS client. Run the Systrap sandbox
with `--filestore-lazy-restore`, `--filestore-adopt-dir=BASE_DIR`, and a root
`--overlay2=root:dir=LOCAL_XFS`. Keep the base generation available for the
lifetime of the sandbox.

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
