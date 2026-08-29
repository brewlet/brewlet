#!/usr/bin/env python3
"""Validate that a registry image reuses a Maven-produced dependency bundle."""

import argparse
import json
import urllib.request

OCI_INDEX = "application/vnd.oci.image.index.v1+json"
OCI_MANIFEST = "application/vnd.oci.image.manifest.v1+json"
OCI_LAYER = "application/vnd.oci.image.layer.v1.tar+gzip"
CONFIG = "application/vnd.brewlet.dependencies.config.v1+json"
LOCK = "application/vnd.brewlet.dependencies.lock.v1+json"


def get(base, repository, kind, reference, accept=None):
    request = urllib.request.Request(
        f"http://{base}/v2/{repository}/{kind}/{reference}")
    if accept:
        request.add_header("Accept", accept)
    with urllib.request.urlopen(request) as response:
        return response.read()


def document(base, repository, descriptor):
    return json.loads(get(base, repository, "blobs", descriptor["digest"]))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("registry")
    parser.add_argument("bundle_repository")
    parser.add_argument("bundle_reference")
    parser.add_argument("application_repository")
    parser.add_argument("application_reference")
    parser.add_argument("source_bom")
    parser.add_argument("dependency")
    args = parser.parse_args()

    bundle = json.loads(get(
        args.registry, args.bundle_repository, "manifests",
        args.bundle_reference, OCI_MANIFEST))
    assert bundle["schemaVersion"] == 2
    assert bundle["mediaType"] == OCI_MANIFEST
    config_descriptor = bundle["config"]
    assert config_descriptor["mediaType"] == CONFIG
    config = document(args.registry, args.bundle_repository, config_descriptor)
    assert config["sourceBom"] == args.source_bom

    dependency_layers = [
        layer for layer in bundle["layers"]
        if layer["mediaType"] == OCI_LAYER
        and layer.get("annotations", {}).get("brewlet.sh/layer") == "classpath"
    ]
    lock_descriptors = [
        layer for layer in bundle["layers"] if layer["mediaType"] == LOCK
    ]
    assert len(dependency_layers) == 1
    assert len(lock_descriptors) == 1
    dependency_layer = dependency_layers[0]
    lock = document(args.registry, args.bundle_repository, lock_descriptors[0])
    expected_group, expected_artifact, expected_version = args.dependency.split(":")
    assert any(
        artifact["groupId"] == expected_group
        and artifact["artifactId"] == expected_artifact
        and artifact["version"] == expected_version
        for artifact in lock["artifacts"]
    )

    image_index = json.loads(get(
        args.registry, args.application_repository, "manifests",
        args.application_reference, OCI_INDEX))
    assert image_index["schemaVersion"] == 2
    assert image_index["mediaType"] == OCI_INDEX
    assert image_index["manifests"]
    for descriptor in image_index["manifests"]:
        manifest = json.loads(get(
            args.registry, args.application_repository, "manifests",
            descriptor["digest"], OCI_MANIFEST))
        matching_layers = [
            layer
            for layer in manifest["layers"]
            if layer["digest"] == dependency_layer["digest"]
        ]
        assert matching_layers == [dependency_layer]


if __name__ == "__main__":
    main()
