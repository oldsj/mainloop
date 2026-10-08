"""Offline compiler contracts. All policy/source authority below is synthetic."""

import copy
import hashlib
import io
import json
import os
import socket
import subprocess  # nosec B404: only used to verify the patched execution tripwire.
import tarfile
import unittest
from contextlib import ExitStack, contextmanager
from pathlib import Path
from unittest.mock import patch

import httpx
from mainloop.environments import build_inputs as compiler
from mainloop.environments.build_inputs import (
    AptSource,
    BuildInputError,
    PackageBuildPolicy,
    ParentSourceReceipt,
    PermittedPackage,
    prepare_package_build,
)
from mainloop.environments.registry import VALIDATOR_VERSION

from models.environment import EnvironmentVersion, PackageDeclaration, PackageSpec

# Construct deliberately fake userinfo for rejection tests, without storing a
# credential-shaped literal that the offline secret scanner would report.
FAKE_CREDENTIAL_URI = (
    "https://"
    + ":".join(("synthetic-user", "synthetic-password"))
    + "@packages.example/debian"
)


def canonical(value):
    return json.dumps(
        value, sort_keys=True, separators=(",", ":"), ensure_ascii=False
    ).encode("utf-8")


def digest(value):
    return "sha256:" + hashlib.sha256(value).hexdigest()


def fake_digest(letter):
    return "sha256:" + letter * 64


def seal_receipt(receipt):
    value = receipt.model_dump(mode="json", exclude={"inventory_digest"})
    for source in value["sources"]:
        source["suites"].sort()
        source["components"].sort()
    value["sources"].sort(key=canonical)
    return receipt.model_copy(update={"inventory_digest": digest(canonical(value))})


def seal_policy(policy):
    value = policy.model_dump(mode="json", exclude={"content_digest"})
    for package in value["packages"]:
        package["requested_versions"].sort(key=canonical)
    value["packages"].sort(key=lambda package: package["name"])
    return policy.model_copy(update={"content_digest": digest(canonical(value))})


def fixture():
    parent = EnvironmentVersion(
        id="synthetic-version",
        environment_id="synthetic-environment",
        registry="ghcr.io",
        repository="example/synthetic-parent",
        index_digest=fake_digest("a"),
        platform_manifest_digest=fake_digest("b"),
        config_digest=fake_digest("c"),
        declared_user="65532:65532",
        architecture="arm64",
        validation_status="static_validated",
        validation_result={"probes": "not_run"},
        validator_version=VALIDATOR_VERSION,
        provenance_kind="user_pushed",
    )
    receipt = seal_receipt(
        ParentSourceReceipt(
            platform_manifest_digest=parent.platform_manifest_digest,
            config_digest=parent.config_digest,
            platform="linux/arm64",
            distribution="debian",
            distribution_version="12",
            apt_version="2.6.1",
            sources=(
                AptSource(
                    uri="https://packages.example/debian",
                    suites=("bookworm", "bookworm-updates"),
                    components=("main",),
                    keyring_path="/usr/share/keyrings/synthetic.gpg",
                    keyring_digest=fake_digest("d"),
                ),
            ),
            inventory_digest=fake_digest("0"),
        )
    )
    policy = seal_policy(
        PackageBuildPolicy(
            id="synthetic-policy",
            version=1,
            parent_environment_id=parent.environment_id,
            parent_version_id=parent.id,
            parent_image=f"{parent.registry}/{parent.repository}@{parent.platform_manifest_digest}",
            parent_config_digest=parent.config_digest,
            platform="linux/arm64",
            manager="apt",
            source_inventory_digest=receipt.inventory_digest,
            packages=(
                PermittedPackage(name="jq", requested_versions=(None, "1.6-2.1")),
                PermittedPackage(
                    name="libfoo++1", requested_versions=("2:1.0~rc1-2+deb12u1",)
                ),
            ),
            content_digest=fake_digest("0"),
        )
    )
    declaration = PackageDeclaration(
        manager="apt",
        packages=[
            PackageSpec(name="libfoo++1", requested_version="2:1.0~rc1-2+deb12u1"),
            PackageSpec(name="jq", requested_version="1.6-2.1"),
        ],
        policy_id=policy.id,
        policy_version=policy.version,
        project_id="synthetic-project",
        task_id="synthetic-task",
    )
    return parent, declaration, policy, receipt


@contextmanager
def no_external_effects():
    # Patch actual effect entry points rather than a fake compiler dependency.
    with ExitStack() as stack:
        # Restore environment only after the putenv/getitem tripwires are removed.
        stack.enter_context(patch.dict(os.environ, {}, clear=True))
        for target in (
            "builtins.open",
            "io.open",
            "os.open",
            "os.getenv",
            "os.putenv",
            "os.system",
            "os.urandom",
            "pathlib.Path.read_text",
            "pathlib.Path.read_bytes",
            "pathlib.Path.write_text",
            "pathlib.Path.write_bytes",
            "socket.socket",
            "socket.getaddrinfo",
            "subprocess.Popen",
            "subprocess.run",
            "httpx.Client",
            "httpx.AsyncClient",
            "asyncpg.connect",
            "asyncpg.create_pool",
            "mainloop.environments.registry.AnonymousOCIRegistry.read",
        ):
            stack.enter_context(
                patch(target, side_effect=AssertionError("external effect: " + target))
            )
        # A direct environ lookup is also an error; clearing alone would miss get().
        stack.enter_context(
            patch.object(
                type(os.environ),
                "__getitem__",
                side_effect=AssertionError("ambient environment"),
            )
        )
        yield


class PackageBuildInputTests(unittest.TestCase):
    def setUp(self):
        self.parent, self.declaration, self.policy, self.receipt = fixture()

    def prepare(self, **changes):
        values = dict(
            parent=self.parent,
            declaration=self.declaration,
            policy=self.policy,
            parent_source_receipt=self.receipt,
        )
        values.update(changes)
        before = copy.deepcopy(values)
        with no_external_effects():
            result = prepare_package_build(**values)
        self.assertEqual(values, before)
        return result

    def reject(self, code, **changes):
        values = dict(
            parent=self.parent,
            declaration=self.declaration,
            policy=self.policy,
            parent_source_receipt=self.receipt,
        )
        values.update(changes)
        before = copy.deepcopy(values)
        with no_external_effects(), self.assertRaises(BuildInputError) as caught:
            prepare_package_build(**values)
        self.assertEqual(caught.exception.code, code)
        self.assertEqual(str(caught.exception), code)
        self.assertFalse(hasattr(caught.exception, "context"))
        self.assertEqual(values, before)

    def test_recipe_manifest_and_safe_context(self):
        result = self.prepare()
        self.assertFalse(result.execution_authorized)
        self.assertEqual(result.context_digest, digest(result.context))
        self.assertEqual(result.manifest_digest, digest(result.manifest))
        manifest = json.loads(result.manifest)
        self.assertEqual(result.manifest, canonical(manifest))
        self.assertEqual(
            manifest["request_digest"], digest(canonical(manifest["request"]))
        )
        self.assertEqual(manifest["context_digest"], result.context_digest)
        self.assertEqual(manifest["parent"]["index_digest"], self.parent.index_digest)
        self.assertEqual(manifest["parent"]["config_digest"], self.parent.config_digest)
        self.assertEqual(
            manifest["source_inventory_digest"],
            digest(canonical(manifest["source_inventory"])),
        )
        self.assertEqual(
            manifest["policy"]["content_digest"], self.policy.content_digest
        )
        self.assertEqual(manifest["request"]["project_id"], "synthetic-project")
        self.assertFalse(manifest["execution_authorized"])
        for forbidden in (
            "resolved_version",
            "approval_reference",
            "runtime_qualified",
            "published",
            "credential",
        ):
            self.assertNotIn(forbidden.encode(), result.manifest)
        with tarfile.open(fileobj=io.BytesIO(result.context)) as archive:
            self.assertEqual(archive.getnames(), ["Dockerfile"])
            entry = archive.getmember("Dockerfile")
            self.assertTrue(entry.isfile())
            self.assertEqual(
                (entry.uid, entry.gid, entry.mtime, entry.mode), (0, 0, 0, 0o644)
            )
            lines = archive.extractfile(entry).read().decode().splitlines()
        self.assertEqual(
            lines[0],
            f"FROM --platform=linux/arm64 ghcr.io/example/synthetic-parent@{self.parent.platform_manifest_digest}",
        )
        self.assertNotIn(self.parent.index_digest, lines[0])
        self.assertEqual(lines[1], "USER 0:0")
        self.assertEqual(lines[-1], "USER 65532:65532")
        commands = [json.loads(line.removeprefix("RUN ")) for line in lines[2:-1]]
        self.assertEqual(
            commands[0],
            ["/usr/bin/apt-get", "-o", "APT::Update::Error-Mode=any", "update"],
        )
        self.assertEqual(
            commands[1],
            [
                "/bin/sh",
                "-ec",
                compiler.EXACT_PACKAGE_CHECK,
                "mainloop-exact-package-check",
                "?exact-name(jq)=1.6-2.1",
                "jq",
                "?exact-name(libfoo++1)=2:1.0~rc1-2+deb12u1",
                "libfoo++1",
            ],
        )
        self.assertEqual(
            commands[2],
            [
                "/usr/bin/env",
                "DEBIAN_FRONTEND=noninteractive",
                "/usr/bin/apt-get",
                "--yes",
                "--no-install-recommends",
                "--no-remove",
                "--no-upgrade",
                "install",
                "--",
                "?exact-name(jq)=1.6-2.1",
                "?exact-name(libfoo++1)=2:1.0~rc1-2+deb12u1",
            ],
        )
        self.assertEqual(
            commands[3],
            [
                "/bin/sh",
                "-c",
                "/usr/bin/apt-get clean && /bin/rm -rf /var/lib/apt/lists/*",
            ],
        )
        with self.assertRaises(ValueError):
            result.execution_authorized = True

    def test_permutations_and_explicit_unpinned_policy(self):
        baseline = self.prepare()
        declaration = self.declaration.model_copy(
            update={"packages": list(reversed(self.declaration.packages))}
        )
        policy = self.policy.model_copy(
            update={"packages": tuple(reversed(self.policy.packages))}
        )
        source = self.receipt.sources[0].model_copy(
            update={"suites": tuple(reversed(self.receipt.sources[0].suites))}
        )
        receipt = self.receipt.model_copy(update={"sources": (source,)})
        self.assertEqual(
            baseline,
            self.prepare(
                declaration=declaration, policy=policy, parent_source_receipt=receipt
            ),
        )
        unpinned = self.declaration.model_copy(
            update={"packages": [PackageSpec(name="jq")]}
        )
        result = self.prepare(declaration=unpinned)
        self.assertIsNone(
            json.loads(result.manifest)["request"]["packages"][0]["requested_version"]
        )
        pinned_only = seal_policy(
            self.policy.model_copy(
                update={
                    "packages": (
                        PermittedPackage(name="jq", requested_versions=("1.6-2.1",)),
                    )
                }
            )
        )
        self.reject("package_not_permitted", declaration=unpinned, policy=pinned_only)

    def test_policy_permitted_dotted_names_use_literal_selectors(self):
        # Actual F1 reproduction: jq. is permitted and explicitly unpinned.
        packages = [
            PackageSpec(name="jq."),
            PackageSpec(name="libfoo++1.2", requested_version="2:1.0~rc1-2+deb12u1"),
        ]
        policy = seal_policy(
            self.policy.model_copy(
                update={
                    "packages": tuple(
                        PermittedPackage(
                            name=p.name, requested_versions=(p.requested_version,)
                        )
                        for p in packages
                    )
                }
            )
        )
        declaration = self.declaration.model_copy(update={"packages": packages})
        result = self.prepare(declaration=declaration, policy=policy)
        with tarfile.open(fileobj=io.BytesIO(result.context)) as archive:
            lines = archive.extractfile("Dockerfile").read().decode().splitlines()
        commands = [json.loads(line.removeprefix("RUN ")) for line in lines[2:-1]]
        selectors = [
            "?exact-name(jq.)",
            "?exact-name(libfoo++1.2)=2:1.0~rc1-2+deb12u1",
        ]
        self.assertEqual(commands[2][-2:], selectors)
        self.assertEqual(
            commands[1],
            [
                "/bin/sh",
                "-ec",
                compiler.EXACT_PACKAGE_CHECK,
                "mainloop-exact-package-check",
                selectors[0],
                "jq.",
                selectors[1],
                "libfoo++1.2",
            ],
        )
        # The check is identical for every request; values are only argv data.
        baseline = self.prepare()
        with tarfile.open(fileobj=io.BytesIO(baseline.context)) as archive:
            baseline_lines = (
                archive.extractfile("Dockerfile").read().decode().splitlines()
            )
        self.assertEqual(commands[1][2], json.loads(baseline_lines[3][4:])[2])
        self.assertNotIn("jq.", commands[1][2])
        self.assertFalse(result.execution_authorized)
        self.assertIsNone(
            json.loads(result.manifest)["request"]["packages"][0]["requested_version"]
        )
        for name in ("jq-extra", "unrelated-libjq-helper"):
            self.reject(
                "package_not_permitted",
                declaration=declaration.model_copy(
                    update={"packages": [PackageSpec(name=name)]}
                ),
                policy=policy,
            )
        for name in ("jq.*", "jq?", "jq)", "?exact-name(jq)", "jq=1.6", "jq:arm64"):
            self.reject(
                "invalid_package_operand" if name == "jq:arm64" else "invalid_request",
                declaration=declaration.model_copy(
                    update={"packages": [PackageSpec.model_construct(name=name)]}
                ),
                policy=policy,
            )
        for version in ("1.*", "1?", "1[0-9]", "1/unstable"):
            self.reject(
                "invalid_request",
                declaration=declaration.model_copy(
                    update={
                        "packages": [
                            PackageSpec.model_construct(
                                name="jq.", requested_version=version
                            )
                        ]
                    }
                ),
                policy=policy,
            )
        for apt_version in ("1.9.9", "3:1.9~rc1"):
            self.reject(
                "unsupported_apt_parent",
                parent_source_receipt=self.receipt.model_copy(
                    update={"apt_version": apt_version}
                ),
            )

    def test_missing_constructed_required_fields_are_bounded(self):
        # Actual F3 reproduction formerly raised AttributeError before validation.
        self.reject("invalid_request", declaration=PackageDeclaration.model_construct())
        for model, field, code in (
            (EnvironmentVersion, "parent", "invalid_parent"),
            (PackageBuildPolicy, "policy", "invalid_policy"),
            (ParentSourceReceipt, "parent_source_receipt", "invalid_source_receipt"),
        ):
            with self.subTest(model=model.__name__):
                self.reject(code, **{field: model.model_construct()})
        self.reject(
            "invalid_request",
            declaration=self.declaration.model_copy(
                update={"packages": [PackageSpec.model_construct()]}
            ),
        )

    def test_identity_tracks_all_authoritative_inputs(self):
        baseline = self.prepare()
        parent = self.parent.model_copy(
            update={
                "id": "other-version",
                "platform_manifest_digest": fake_digest("e"),
                "config_digest": fake_digest("f"),
            }
        )
        receipt = seal_receipt(
            self.receipt.model_copy(
                update={
                    "platform_manifest_digest": parent.platform_manifest_digest,
                    "config_digest": parent.config_digest,
                }
            )
        )
        policy = seal_policy(
            self.policy.model_copy(
                update={
                    "parent_version_id": parent.id,
                    "parent_image": f"{parent.registry}/{parent.repository}@{parent.platform_manifest_digest}",
                    "parent_config_digest": parent.config_digest,
                    "source_inventory_digest": receipt.inventory_digest,
                }
            )
        )
        changed = self.prepare(
            parent=parent, policy=policy, parent_source_receipt=receipt
        )
        self.assertNotEqual(baseline.context_digest, changed.context_digest)
        self.assertNotEqual(baseline.manifest_digest, changed.manifest_digest)
        source = self.receipt.sources[0].model_copy(
            update={"keyring_digest": fake_digest("e")}
        )
        receipt = seal_receipt(self.receipt.model_copy(update={"sources": (source,)}))
        policy = seal_policy(
            self.policy.model_copy(
                update={"source_inventory_digest": receipt.inventory_digest}
            )
        )
        self.assertNotEqual(
            baseline.manifest_digest,
            self.prepare(policy=policy, parent_source_receipt=receipt).manifest_digest,
        )
        policy = seal_policy(self.policy.model_copy(update={"version": 2}))
        declaration = self.declaration.model_copy(update={"policy_version": 2})
        self.assertNotEqual(
            baseline.manifest_digest,
            self.prepare(policy=policy, declaration=declaration).manifest_digest,
        )
        self.assertNotEqual(
            baseline.manifest_digest,
            self.prepare(
                parent=self.parent.model_copy(update={"index_digest": fake_digest("e")})
            ).manifest_digest,
        )
        declaration = self.declaration.model_copy(update={"task_id": "other-task"})
        self.assertNotEqual(
            baseline.request_digest,
            self.prepare(declaration=declaration).request_digest,
        )
        with patch.object(compiler, "GENERATOR_VERSION", "apt-inputs-test-next"):
            self.assertNotEqual(
                baseline.manifest_digest, self.prepare().manifest_digest
            )

    def test_unapproved_names_versions_and_duplicates(self):
        for package in (
            PackageSpec(name="curl"),
            PackageSpec(name="jq", requested_version="9.9"),
        ):
            self.reject(
                "package_not_permitted",
                declaration=self.declaration.model_copy(update={"packages": [package]}),
            )
        for duplicate in (
            PackageSpec(name="jq", requested_version="1.6-2.1"),
            PackageSpec(name="jq"),
        ):
            self.reject(
                "duplicate_package",
                declaration=self.declaration.model_copy(
                    update={"packages": [self.declaration.packages[1], duplicate]}
                ),
            )

    def test_injection_extra_fields_and_forged_evidence(self):
        for name in (
            "--yes",
            "-oAPT::Get::AllowUnauthenticated=true",
            "jq arm64",
            "jq\nRUN evil",
            "jq:arm64",
            "https://evil.example/jq",
            "git+https://evil.example/x",
            "jq*",
            "jq-",
            "jq+",
            "jq.",
        ):
            declaration = self.declaration.model_dump(exclude_unset=True)
            declaration["packages"] = [{"name": name}]
            # apt's trailing '-'/'+' selectors are rejected even if policy names them.
            expected = (
                "invalid_package_operand"
                if name in ("jq:arm64", "jq-", "jq+")
                else "package_not_permitted" if name == "jq." else "invalid_request"
            )
            self.reject(expected, declaration=declaration)
        for version in (
            "--yes",
            "latest",
            "1*",
            "1\nUSER 0",
            "1;evil",
            "https://evil.example",
            "1:1:1",
            "1/../../file",
            "1-",
            "1_2",
        ):
            declaration = self.declaration.model_dump(exclude_unset=True)
            declaration["packages"] = [{"name": "jq", "requested_version": version}]
            expected = (
                "invalid_package_operand"
                if version in ("--yes", "latest", "1:1:1", "1-", "1_2")
                else "invalid_request"
            )
            self.reject(expected, declaration=declaration)
        for extra in ("run", "build_args", "credentials", "runtime_command"):
            declaration = self.declaration.model_dump(exclude_unset=True)
            declaration[extra] = "forged"
            self.reject("invalid_request", declaration=declaration)
        for changes in (
            {"repositories": ["https://evil.example"]},
            {"metadata_hashes": [fake_digest("a")]},
            {"metadata_hashes": []},
            {"generator_version": None},
            {"generator_version": "forged"},
        ):
            self.reject(
                (
                    "repositories_override"
                    if "repositories" in changes
                    else "caller_evidence"
                ),
                declaration=self.declaration.model_copy(update=changes),
            )
        for resolved in ("1.6-2.1", None):
            self.reject(
                "caller_evidence",
                declaration=self.declaration.model_copy(
                    update={
                        "packages": [PackageSpec(name="jq", resolved_version=resolved)]
                    }
                ),
            )

    def test_request_bounds_and_malformed_models(self):
        for field, value in (("name", "a" * 129), ("requested_version", "1" * 129)):
            package = {"name": "jq", field: value}
            self.reject(
                "package_string_limit",
                declaration=self.declaration.model_copy(
                    update={"packages": [PackageSpec(**package)]}
                ),
            )
        packages = [PackageSpec(name=f"pkg-{index}") for index in range(33)]
        self.reject(
            "too_many_packages",
            declaration=self.declaration.model_copy(update={"packages": packages}),
        )
        self.reject(
            "request_byte_limit",
            declaration=self.declaration.model_copy(update={"project_id": "x" * 17000}),
        )
        self.reject(
            "invalid_request_scope",
            declaration=self.declaration.model_copy(
                update={"task_id": FAKE_CREDENTIAL_URI}
            ),
        )
        self.reject(
            "invalid_request",
            declaration=self.declaration.model_copy(update={"task_id": "\ud800"}),
        )
        for changes in (
            {"packages": []},
            {"packages": [PackageSpec.model_construct(name="jq\nRUN evil")]},
            {"policy_version": True},
            {"policy_version": "1"},
            {"unexpected": "field"},
        ):
            self.reject(
                "invalid_request",
                declaration=self.declaration.model_copy(update=changes),
            )
        declaration = self.declaration.model_dump(exclude_unset=True)
        declaration["packages"] = [
            PackageSpec(name="jq").model_copy(update={"credentials": "forged"})
        ]
        self.reject("invalid_request", declaration=declaration)
        name, version = "a" * 128, "1" * 128
        packages = [PackageSpec(name=name, requested_version=version)] + [
            PackageSpec(name=f"pkg-{i}") for i in range(31)
        ]
        policy = seal_policy(
            self.policy.model_copy(
                update={
                    "packages": tuple(
                        PermittedPackage(
                            name=p.name, requested_versions=(p.requested_version,)
                        )
                        for p in packages
                    )
                }
            )
        )
        self.prepare(
            declaration=self.declaration.model_copy(update={"packages": packages}),
            policy=policy,
        )

    def test_trusted_inputs_required_and_content_verified(self):
        for field, code in (
            ("policy", "invalid_policy"),
            ("parent_source_receipt", "invalid_source_receipt"),
        ):
            for value in (None, {}, {"policy_id": "actor-claim"}):
                self.reject(code, **{field: value})
        self.reject(
            "invalid_policy", policy=self.policy.model_copy(update={"packages": ()})
        )
        self.reject(
            "invalid_source_receipt",
            parent_source_receipt=self.receipt.model_copy(update={"sources": ()}),
        )
        self.reject(
            "policy_content_digest_mismatch",
            policy=self.policy.model_copy(update={"content_digest": fake_digest("f")}),
        )
        self.reject(
            "source_inventory_digest_mismatch",
            parent_source_receipt=self.receipt.model_copy(
                update={"apt_version": "2.6.2"}
            ),
        )
        receipt = seal_receipt(self.receipt.model_copy(update={"apt_version": "2.6.2"}))
        self.reject("policy_source_mismatch", parent_source_receipt=receipt)
        self.reject(
            "duplicate_source",
            parent_source_receipt=self.receipt.model_copy(
                update={"sources": self.receipt.sources * 2}
            ),
        )
        self.reject(
            "duplicate_policy_package",
            policy=self.policy.model_copy(
                update={"packages": self.policy.packages * 2}
            ),
        )
        for uri in (
            FAKE_CREDENTIAL_URI,
            "https://packages.example/debian?token=secret",
            "https://packages.example/debian#secret",
            "file:///etc/passwd",
            "https://packages.example/../debian",
            "https://packages.example/debian\n",
        ):
            source = self.receipt.sources[0].model_copy(update={"uri": uri})
            self.reject(
                "invalid_source_receipt",
                parent_source_receipt=self.receipt.model_copy(
                    update={"sources": (source,)}
                ),
            )

    def test_parent_policy_receipt_binding_and_unsupported_profiles(self):
        self.reject(
            "stale_parent_validator",
            parent=self.parent.model_copy(
                update={"validator_version": "oci-static-v1"}
            ),
        )
        self.reject(
            "parent_not_static_validated",
            parent=self.parent.model_copy(
                update={"validation_status": "pending_build"}
            ),
        )
        self.reject(
            "invalid_parent",
            parent=self.parent.model_copy(update={"declared_user": "0:0"}),
        )
        self.reject(
            "invalid_parent",
            parent=self.parent.model_copy(
                update={"platform_manifest_digest": "latest"}
            ),
        )
        for repository in (
            "example/parent:latest",
            "../parent",
            "example//parent",
            "example/parent\n",
        ):
            self.reject(
                "invalid_parent_reference",
                parent=self.parent.model_copy(update={"repository": repository}),
            )
        for field, value in (
            ("parent_environment_id", "foreign-env"),
            ("parent_version_id", "foreign-version"),
            ("parent_image", "ghcr.io/example/parent:latest"),
            ("parent_config_digest", fake_digest("f")),
        ):
            self.reject(
                "policy_parent_mismatch",
                policy=self.policy.model_copy(update={field: value}),
            )
        for changes in ({"policy_id": "foreign-policy"}, {"policy_version": 2}):
            self.reject(
                "request_policy_mismatch",
                declaration=self.declaration.model_copy(update=changes),
            )
        for field in ("platform_manifest_digest", "config_digest"):
            self.reject(
                "receipt_parent_mismatch",
                parent_source_receipt=self.receipt.model_copy(
                    update={field: fake_digest("f")}
                ),
            )
        for changes in ({"architecture": "amd64"},):
            self.reject(
                "unsupported_platform", parent=self.parent.model_copy(update=changes)
            )
        for field in ("policy", "parent_source_receipt"):
            original = self.policy if field == "policy" else self.receipt
            self.reject(
                "unsupported_platform",
                **{field: original.model_copy(update={"platform": "darwin/arm64"})},
            )
        self.reject(
            "unsupported_manager",
            declaration=self.declaration.model_copy(update={"manager": "apk"}),
        )
        self.reject(
            "unsupported_manager",
            policy=self.policy.model_copy(update={"manager": "apk"}),
        )
        for changes in ({"distribution": "alpine"}, {"apt_version": None}):
            self.reject(
                "unsupported_apt_parent",
                parent_source_receipt=self.receipt.model_copy(update=changes),
            )

    def test_tripwires_cover_effectful_entry_points(self):
        for operation in (
            lambda: Path("/not-a-real-credential").read_text(),
            lambda: os.environ.get("UNTRUSTED_POLICY"),
            lambda: socket.getaddrinfo("example.invalid", 443),
            lambda: subprocess.run(
                ["/not-a-real-build"], check=True
            ),  # nosec B603: mocked by no_external_effects.
            lambda: httpx.Client(),
        ):
            with no_external_effects(), self.assertRaises(AssertionError):
                operation()


if __name__ == "__main__":
    unittest.main()
