#!/usr/bin/env python3

# Copyright 2026 The gVisor Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Stage exact candidate bytes for isolated CI; never publish these pins."""

import hashlib
import json
import re
from pathlib import Path
import sys
from urllib.parse import urlsplit


def main():
    if len(sys.argv) != 6:
        raise SystemExit("usage: MANIFEST BUNDLE_DIR SOURCE_ENV OUTPUT_ENV BASE_URL")
    manifest_path, bundles, source, output = map(Path, sys.argv[1:5])
    base_url = sys.argv[5]
    url = urlsplit(base_url)
    if url.scheme != "http" or not url.hostname or url.username or url.password:
        raise SystemExit("qualification requires an unauthenticated local HTTP server")
    manifest = json.loads(manifest_path.read_text())
    if manifest["schema"] != 4 or manifest["repository"] != "akernel-dev/gvisor":
        raise SystemExit("unexpected candidate provenance")
    if not re.fullmatch(r"[0-9a-f]{40}", manifest["commit"]):
        raise SystemExit("invalid candidate source commit")
    if not re.fullmatch(r"release-\d{8}\.\d+-akernel\.[1-9]\d*", manifest["release_tag"]):
        raise SystemExit("invalid candidate release tag")

    values = {"GVISOR_RELEASE": manifest["release_tag"]}
    for arch in ("amd64", "arm64"):
        entry = manifest["archives"][arch]
        name = f"gvisor-{arch}.tar.bz2"
        if entry["name"] != name or not re.fullmatch(r"[0-9a-f]{128}", entry["sha512"]):
            raise SystemExit(f"invalid {arch} archive identity")
        digest = hashlib.sha512()
        with (bundles / name).open("rb") as stream:
            for chunk in iter(lambda: stream.read(1024 * 1024), b""):
                digest.update(chunk)
        if digest.hexdigest() != entry["sha512"]:
            raise SystemExit(f"{arch} candidate checksum mismatch")
        values[f"GVISOR_{arch.upper()}_SHA512"] = entry["sha512"]
        values[f"GVISOR_{arch.upper()}_URL"] = f"{base_url}/{name}"

    counts = dict.fromkeys(values, 0)
    lines = ["# Qualification only: temporary candidate pins; never publish this file."]
    for line in source.read_text().splitlines():
        key = line.split("=", 1)[0]
        if key in values:
            counts[key] += 1
            line = f"{key}={values[key]}"
        lines.append(line)
    if any(count != 1 for count in counts.values()):
        raise SystemExit("source runtime manifest must contain each gVisor pin exactly once")
    output.write_text("\n".join(lines) + "\n")


if __name__ == "__main__":
    main()
