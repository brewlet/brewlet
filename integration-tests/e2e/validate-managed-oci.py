#!/usr/bin/env python3
"""Validate the normative managed-dependency OCI wire contract."""

import argparse
import base64
import gzip
import hashlib
import io
import json
import re
import tarfile
import urllib.parse
from pathlib import Path

OCI_MANIFEST = "application/vnd.oci.image.manifest.v1+json"
OCI_INDEX = "application/vnd.oci.image.index.v1+json"
OCI_LAYER = "application/vnd.oci.image.layer.v1.tar+gzip"
BUNDLE = "application/vnd.brewlet.dependencies.v1+json"
CONFIG = "application/vnd.brewlet.dependencies.config.v1+json"
LOCK = "application/vnd.brewlet.dependencies.lock.v1+json"
CYCLONEDX = "application/vnd.cyclonedx+json"
ATTESTATION = "application/vnd.brewlet.attestation.v1+json"
DSSE = "application/vnd.dsse.envelope.v1+json"
EMPTY_CONFIG = "application/vnd.oci.empty.v1+json"
BUNDLE_PREDICATE = "https://brewlet.sh/attestations/dependency-bundle/v1"
MANAGED_PREDICATE = "https://brewlet.sh/attestations/managed-dependencies/v1"


def digest(data):
    return "sha256:" + hashlib.sha256(data).hexdigest()


class Layout:
    def __init__(self, root):
        self.root = Path(root)
        self.index = json.loads((self.root / "index.json").read_text())
        assert self.index["schemaVersion"] == 2
        assert self.index["mediaType"] == OCI_INDEX

    def blob(self, descriptor):
        assert descriptor["digest"].startswith("sha256:")
        data = (self.root / "blobs" / "sha256" /
                descriptor["digest"][7:]).read_bytes()
        assert len(data) == descriptor["size"]
        assert digest(data) == descriptor["digest"]
        return data

    def tagged(self, ref):
        matches = [
            descriptor for descriptor in self.index["manifests"]
            if descriptor.get("annotations", {}).get(
                "org.opencontainers.image.ref.name") == ref
        ]
        assert len(matches) == 1
        return matches[0]

    def referrers(self, subject, artifact_type):
        return [
            descriptor for descriptor in self.index["manifests"]
            if descriptor.get("artifactType") == artifact_type
            and descriptor.get("annotations", {}).get(
                "brewlet.sh/referrer-subject") == subject["digest"]
        ]

    def referrer_document(self, descriptor, subject, layer_type):
        assert descriptor["mediaType"] == OCI_MANIFEST
        manifest = json.loads(self.blob(descriptor))
        assert manifest["schemaVersion"] == 2
        assert manifest["mediaType"] == OCI_MANIFEST
        assert manifest["artifactType"] == descriptor["artifactType"]
        assert manifest["subject"] == {
            "mediaType": subject["mediaType"],
            "digest": subject["digest"],
            "size": subject["size"],
        }
        config = manifest["config"]
        assert config["mediaType"] == EMPTY_CONFIG
        assert self.blob(config) == b"{}"
        assert len(manifest["layers"]) == 1
        layer = manifest["layers"][0]
        assert layer["mediaType"] == layer_type
        return self.blob(layer), manifest


def quote(value):
    return urllib.parse.quote_plus(value, safe="").replace("+", "%20")


def purl(artifact):
    value = (f"pkg:maven/{quote(artifact['groupId'])}/"
             f"{quote(artifact['artifactId'])}@{quote(artifact['version'])}")
    qualifiers = []
    if artifact["type"] != "jar":
        qualifiers.append("type=" + quote(artifact["type"]))
    if artifact.get("classifier"):
        qualifiers.append("classifier=" + quote(artifact["classifier"]))
    return value + ("?" + "&".join(qualifiers) if qualifiers else "")


def statement(envelope_bytes, predicate_type, subject_digest):
    envelope = json.loads(envelope_bytes)
    assert set(envelope) == {"payloadType", "payload", "signatures"}
    assert envelope["payloadType"] == "application/vnd.in-toto+json"
    assert len(envelope["signatures"]) == 1
    signature = envelope["signatures"][0]
    assert re.fullmatch(r"sha256:[0-9a-f]{64}", signature["keyid"])
    assert base64.b64decode(signature["sig"])[0] == 0x30
    payload = json.loads(base64.b64decode(envelope["payload"]))
    assert payload["_type"] == "https://in-toto.io/Statement/v1"
    assert payload["predicateType"] == predicate_type
    assert len(payload["subject"]) == 1
    assert payload["subject"][0]["digest"] == {
        "sha256": subject_digest[7:]
    }
    return payload["predicate"]


def validate_bundle(layout, ref, require_signature):
    subject = layout.tagged(ref)
    assert subject["mediaType"] == OCI_MANIFEST
    assert subject["artifactType"] == BUNDLE
    manifest = json.loads(layout.blob(subject))
    assert manifest["schemaVersion"] == 2
    assert manifest["mediaType"] == OCI_MANIFEST
    assert manifest["artifactType"] == BUNDLE
    assert set(manifest) == {
        "schemaVersion", "mediaType", "artifactType", "config", "layers"
    }

    config_descriptor = manifest["config"]
    assert config_descriptor["mediaType"] == CONFIG
    config = json.loads(layout.blob(config_descriptor))
    required_config = {
        "schemaVersion", "name", "version", "sourceBom", "lockDigest",
        "layerDigest", "layerDiffId"
    }
    assert required_config <= set(config) <= required_config | {
        "compatibleJdks"
    }
    assert config["schemaVersion"] == 1
    assert config["name"] and config["version"]
    assert len(config["sourceBom"].split(":")) == 3
    for key in ("lockDigest", "layerDigest", "layerDiffId"):
        assert re.fullmatch(r"sha256:[0-9a-f]{64}", config[key])
    jdks = config.get("compatibleJdks", [])
    assert jdks == sorted(set(jdks)) and all(jdk > 0 for jdk in jdks)

    layers = manifest["layers"]
    assert len(layers) == 2
    dependency_layers = [
        value for value in layers
        if value["mediaType"] == OCI_LAYER
        and value.get("annotations", {}).get("brewlet.sh/layer") == "classpath"
    ]
    locks = [value for value in layers if value["mediaType"] == LOCK]
    assert len(dependency_layers) == 1 and len(locks) == 1
    dependency_layer, lock_descriptor = dependency_layers[0], locks[0]
    assert dependency_layer["digest"] == config["layerDigest"]
    assert lock_descriptor["digest"] == config["lockDigest"]

    lock = json.loads(layout.blob(lock_descriptor))
    assert set(lock) == {"schemaVersion", "artifacts"}
    assert lock["schemaVersion"] == 1 and lock["artifacts"]
    coordinates, filenames = [], []
    for artifact in lock["artifacts"]:
        required = {
            "groupId", "artifactId", "version", "type", "scope",
            "fileName", "sha256"
        }
        assert set(artifact) in (required, required | {"classifier"})
        if "classifier" in artifact:
            assert artifact["classifier"]
        assert all(artifact[key] for key in required - {"sha256"})
        assert "/" not in artifact["fileName"]
        assert artifact["fileName"].lower().endswith(".jar")
        assert re.fullmatch(r"[0-9a-f]{64}", artifact["sha256"])
        coordinates.append(":".join([
            artifact["groupId"], artifact["artifactId"], artifact["type"],
            artifact.get("classifier") or "", artifact["version"]
        ]))
        filenames.append(artifact["fileName"])
    assert coordinates == sorted(coordinates)
    assert len(coordinates) == len(set(coordinates))
    assert len(filenames) == len(set(filenames))

    compressed = layout.blob(dependency_layer)
    assert int.from_bytes(compressed[4:8], "little") == 0
    tar_bytes = gzip.decompress(compressed)
    assert digest(tar_bytes) == config["layerDiffId"]
    with tarfile.open(fileobj=io.BytesIO(tar_bytes), mode="r:") as archive:
        members = archive.getmembers()
        assert [member.name for member in members] == sorted(filenames)
        assert all(member.isfile() and member.mode == 0o644
                   and member.uid == 0 and member.gid == 0
                   and member.mtime == 0 for member in members)
        by_name = {artifact["fileName"]: artifact for artifact in lock["artifacts"]}
        for member in members:
            content = archive.extractfile(member).read()
            assert hashlib.sha256(content).hexdigest() == by_name[
                member.name]["sha256"]

    sbom_refs = layout.referrers(subject, CYCLONEDX)
    provenance_refs = layout.referrers(subject, ATTESTATION)
    assert len(sbom_refs) == 1
    if require_signature:
        assert provenance_refs
    sbom_bytes, _ = layout.referrer_document(
        sbom_refs[0], subject, CYCLONEDX)
    sbom = json.loads(sbom_bytes)
    assert sbom["bomFormat"] == "CycloneDX"
    assert sbom["specVersion"] == "1.5" and sbom["version"] == 1
    assert len(sbom["components"]) == len(lock["artifacts"])
    components = {component["purl"]: component
                  for component in sbom["components"]}
    for artifact in lock["artifacts"]:
        component = components[purl(artifact)]
        assert component["group"] == artifact["groupId"]
        assert component["name"] == artifact["artifactId"]
        assert component["version"] == artifact["version"]
        assert component["hashes"] == [{
            "alg": "SHA-256", "content": artifact["sha256"]
        }]

    valid_predicates = []
    for descriptor in provenance_refs:
        try:
            document, referrer = layout.referrer_document(
                descriptor, subject, DSSE)
            assert referrer["annotations"]["brewlet.sh/predicate-type"] == \
                BUNDLE_PREDICATE
            valid_predicates.append(statement(
                document, BUNDLE_PREDICATE, subject["digest"]))
        except (AssertionError, LookupError, OSError, TypeError, UnicodeError,
                ValueError):
            continue
    expected_keys = {
        "schemaVersion", "dependencyBundleDigest", "dependencyLayerDigest",
        "dependencyLockDigest", "sbomDigest", "sourceBom", "builderIdentity"
    }
    if provenance_refs:
        assert any(
            set(predicate) == expected_keys
            and predicate["schemaVersion"] == 1
            and predicate["dependencyBundleDigest"] == subject["digest"]
            and predicate["dependencyLayerDigest"] == config["layerDigest"]
            and predicate["dependencyLockDigest"] == config["lockDigest"]
            and predicate["sbomDigest"] == digest(sbom_bytes)
            and predicate["sourceBom"] == config["sourceBom"]
            and predicate["builderIdentity"]
            for predicate in valid_predicates
        )


def validate_image(layout, ref, require_signature):
    subject = layout.tagged(ref)
    assert subject["mediaType"] == OCI_INDEX
    refs = layout.referrers(subject, ATTESTATION)
    predicates = []
    for descriptor in refs:
        try:
            document, referrer = layout.referrer_document(
                descriptor, subject, DSSE)
            if referrer.get("annotations", {}).get(
                    "brewlet.sh/predicate-type") == MANAGED_PREDICATE:
                predicates.append(statement(
                    document, MANAGED_PREDICATE, subject["digest"]))
        except (AssertionError, LookupError, OSError, TypeError, UnicodeError,
                ValueError):
            continue
    if require_signature:
        assert predicates
    expected_keys = {
        "schemaVersion", "finalImageDigest", "thinJar",
        "applicationJarDigest", "dependencyBundleDigest",
        "dependencyLayerDigest", "dependencyLockDigest", "sbomDigest",
        "sourceBom", "builderIdentity"
    }
    assert not refs or any(
        set(predicate) == expected_keys
        and predicate["schemaVersion"] == 1
        and predicate["thinJar"] is True
        and predicate["finalImageDigest"] == subject["digest"]
        and all(re.fullmatch(r"sha256:[0-9a-f]{64}", predicate[key])
                for key in (
                    "applicationJarDigest", "dependencyBundleDigest",
                    "dependencyLayerDigest", "dependencyLockDigest",
                    "sbomDigest"))
        and len(predicate["sourceBom"].split(":")) == 3
        and predicate["builderIdentity"]
        for predicate in predicates
    )


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("kind", choices=("bundle", "image"))
    parser.add_argument("layout")
    parser.add_argument("ref")
    parser.add_argument("--require-signature", action="store_true")
    args = parser.parse_args()
    layout = Layout(args.layout)
    if args.kind == "bundle":
        validate_bundle(layout, args.ref, args.require_signature)
    else:
        validate_image(layout, args.ref, args.require_signature)


if __name__ == "__main__":
    main()
