#!/usr/bin/env python3
"""Create one intentional integrity failure in a managed bundle OCI layout."""

import argparse
import json
from pathlib import Path


def blob_path(root, digest):
    return root / "blobs" / "sha256" / digest.removeprefix("sha256:")


def flip_last_byte(path):
    data = bytearray(path.read_bytes())
    data[-1] ^= 0xFF
    path.write_bytes(data)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("layout")
    parser.add_argument("ref")
    parser.add_argument("target", choices=("descriptor", "config", "lock", "layer"))
    args = parser.parse_args()

    root = Path(args.layout)
    index_path = root / "index.json"
    index = json.loads(index_path.read_text())
    subject = next(
        descriptor for descriptor in index["manifests"]
        if descriptor.get("annotations", {}).get(
            "org.opencontainers.image.ref.name") == args.ref
    )
    if args.target == "descriptor":
        subject["size"] += 1
        index_path.write_text(json.dumps(index))
        return

    manifest = json.loads(blob_path(root, subject["digest"]).read_text())
    if args.target == "config":
        descriptor = manifest["config"]
    elif args.target == "lock":
        descriptor = next(
            value for value in manifest["layers"]
            if value["mediaType"] ==
            "application/vnd.brewlet.dependencies.lock.v1+json"
        )
    else:
        descriptor = next(
            value for value in manifest["layers"]
            if value["mediaType"] ==
            "application/vnd.oci.image.layer.v1.tar+gzip"
        )
    flip_last_byte(blob_path(root, descriptor["digest"]))


if __name__ == "__main__":
    main()
