#!/usr/bin/env python3
"""Validate that a registry image reuses a Maven-produced dependency bundle."""

import argparse
import base64
import hashlib
import json
import subprocess
import tempfile
import urllib.request
from pathlib import Path

OCI_INDEX = "application/vnd.oci.image.index.v1+json"
OCI_MANIFEST = "application/vnd.oci.image.manifest.v1+json"
OCI_LAYER = "application/vnd.oci.image.layer.v1.tar+gzip"
CONFIG = "application/vnd.brewlet.dependencies.config.v1+json"
LOCK = "application/vnd.brewlet.dependencies.lock.v1+json"
ATTESTATION = "application/vnd.brewlet.attestation.v1+json"
DSSE = "application/vnd.dsse.envelope.v1+json"
EMPTY_CONFIG = "application/vnd.oci.empty.v1+json"
MANAGED_PREDICATE = "https://brewlet.sh/attestations/managed-dependencies/v1"
EVIDENCE_ANNOTATION = "brewlet.sh/managed-dependency-evidence"


def get(base, repository, kind, reference, accept=None):
    request = urllib.request.Request(
        f"http://{base}/v2/{repository}/{kind}/{reference}")
    if accept:
        request.add_header("Accept", accept)
    with urllib.request.urlopen(request) as response:
        return response.read()


def document(base, repository, descriptor):
    return json.loads(get(base, repository, "blobs", descriptor["digest"]))


def digest(data):
    return "sha256:" + hashlib.sha256(data).hexdigest()


def verify_dsse(envelope_bytes, public_key, subject_digest, identity):
    envelope = json.loads(envelope_bytes)
    assert set(envelope) == {"payloadType", "payload", "signatures"}
    assert envelope["payloadType"] == "application/vnd.in-toto+json"
    assert len(envelope["signatures"]) == 1
    payload = base64.b64decode(envelope["payload"], validate=True)
    signature = base64.b64decode(
        envelope["signatures"][0]["sig"], validate=True)
    key_der = subprocess.check_output([
        "openssl", "pkey", "-pubin", "-in", public_key, "-outform", "DER"
    ])
    assert envelope["signatures"][0]["keyid"] == digest(key_der)
    payload_type = envelope["payloadType"].encode()
    pae = (b"DSSEv1 " + str(len(payload_type)).encode() + b" " + payload_type
           + b" " + str(len(payload)).encode() + b" " + payload)
    with tempfile.TemporaryDirectory() as directory:
        root = Path(directory)
        data_path = root / "pae"
        signature_path = root / "signature"
        data_path.write_bytes(pae)
        signature_path.write_bytes(signature)
        verified = subprocess.run([
            "openssl", "dgst", "-sha256", "-verify", public_key,
            "-signature", str(signature_path), str(data_path)
        ], capture_output=True, check=False)
        assert verified.returncode == 0, verified.stderr.decode()

    statement = json.loads(payload)
    assert statement["_type"] == "https://in-toto.io/Statement/v1"
    assert statement["predicateType"] == MANAGED_PREDICATE
    assert statement["subject"] == [{
        "name": statement["subject"][0]["name"],
        "digest": {"sha256": subject_digest[7:]},
    }]
    predicate = statement["predicate"]
    assert predicate["builderIdentity"] == identity
    return predicate


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("registry")
    parser.add_argument("bundle_repository")
    parser.add_argument("bundle_reference")
    parser.add_argument("application_repository")
    parser.add_argument("application_reference")
    parser.add_argument("source_bom")
    parser.add_argument("dependency")
    parser.add_argument("public_key")
    parser.add_argument("builder_identity")
    args = parser.parse_args()

    bundle_bytes = get(
        args.registry, args.bundle_repository, "manifests",
        args.bundle_reference, OCI_MANIFEST)
    bundle = json.loads(bundle_bytes)
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

    image_index_bytes = get(
        args.registry, args.application_repository, "manifests",
        args.application_reference, OCI_INDEX)
    image_index = json.loads(image_index_bytes)
    assert image_index["schemaVersion"] == 2
    assert image_index["mediaType"] == OCI_INDEX
    assert image_index["manifests"]
    evidences = []
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
        evidences.append(json.loads(
            manifest["annotations"][EVIDENCE_ANNOTATION]))
    assert evidences and all(evidence == evidences[0]
                             for evidence in evidences[1:])
    evidence = evidences[0]

    image_digest = digest(image_index_bytes)
    type_hash = hashlib.sha256(ATTESTATION.encode()).hexdigest()[:12]
    prefix = image_digest.replace(":", "-") + "." + type_hash + "."
    tags = json.loads(get(
        args.registry, args.application_repository, "tags", "list"))["tags"]
    candidates = [tag for tag in tags if tag.startswith(prefix)]
    assert len(candidates) == 1
    attestation_bytes = get(
        args.registry, args.application_repository, "manifests",
        candidates[0], OCI_MANIFEST)
    attestation = json.loads(attestation_bytes)
    assert attestation["schemaVersion"] == 2
    assert attestation["mediaType"] == OCI_MANIFEST
    assert attestation["artifactType"] == ATTESTATION
    assert attestation["subject"] == {
        "mediaType": OCI_INDEX,
        "digest": image_digest,
        "size": len(image_index_bytes),
    }
    assert attestation["config"]["mediaType"] == EMPTY_CONFIG
    assert get(args.registry, args.application_repository, "blobs",
               attestation["config"]["digest"]) == b"{}"
    assert len(attestation["layers"]) == 1
    assert attestation["layers"][0]["mediaType"] == DSSE
    envelope = get(args.registry, args.application_repository, "blobs",
                   attestation["layers"][0]["digest"])
    assert digest(envelope) == attestation["layers"][0]["digest"]
    predicate = verify_dsse(
        envelope, args.public_key, image_digest, args.builder_identity)
    assert set(predicate) == {
        "schemaVersion", "finalImageDigest", "thinJar",
        "applicationJarDigest", "dependencyBundleDigest",
        "dependencyLayerDigest", "dependencyLockDigest", "sbomDigest",
        "sourceBom", "builderIdentity",
    }
    assert predicate["schemaVersion"] == 1
    assert predicate["finalImageDigest"] == image_digest
    assert predicate["thinJar"] is True
    assert predicate["dependencyBundleDigest"] == digest(bundle_bytes)
    assert predicate["dependencyLayerDigest"] == config["layerDigest"]
    assert predicate["dependencyLockDigest"] == config["lockDigest"]
    assert predicate["sourceBom"] == args.source_bom
    for key in (
            "thinJar", "applicationJarDigest", "dependencyBundleDigest",
            "dependencyLayerDigest", "dependencyLockDigest", "sbomDigest",
            "sourceBom", "builderIdentity"):
        assert predicate[key] == evidence[key]


if __name__ == "__main__":
    main()
