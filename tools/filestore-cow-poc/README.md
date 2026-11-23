# Host FUSE filestore PoC

This standalone daemon presents one writable and mmap-capable `filestore-0`.
It reads inherited pages from an immutable base, writes modified pages to a
sparse local file at the original offsets, and records holes explicitly.

Build on Linux with Go 1.25.7 or newer.

```sh
cd tools/filestore-cow-poc
go build -o rsi-cowfs .
```

Run in a dedicated host mount namespace with access to `/dev/fuse` and
permission to mount FUSE. Use an empty mount directory, a read-only baseline,
a new delta path on local XFS and a socket in a private directory.

```sh
./rsi-cowfs MOUNTPOINT BASE_FILE LOCAL_DELTA CONTROL_SOCKET
```

Pass the mount directory to the existing runsc restore options
`--filestore-adopt-dir=MOUNTPOINT` and `--filestore-clone-on-adopt=false`. The
sandbox uses the daemon's writable view as its private filestore backing FD.

To collect an increment, pause the sandbox and fsync its host backing FD.
Save the process checkpoint with `--skip-filestore-pages`, then send
`freeze` to the control socket and close the sending half of the connection.
The daemon syncs the local delta and returns a JSON index of the byte ranges
to export. Copy `data` ranges out of the local delta and apply `zero` ranges as
holes in the next generation. The frozen view is read-only. Stop the sandbox
and unmount the view before discarding its local files.

The daemon also accepts a previous increment as two optional arguments,
`PRIOR_INDEX PRIOR_DATA`, for the earlier chained-delta experiment. When
the publisher produces a complete generation, the new `filestore-0` can be
used directly as the next base.

The daemon keeps its live extent index in memory. The saved extent index is
emitted only at freeze time.
