"""Definite creation refusals, bounded diagnostics and legacy PostgreSQL recovery."""

import asyncio
import base64
import binascii
import json
import re
import unittest
from unittest.mock import patch
from urllib.parse import unquote

import httpx
from mainloop.db import db
from mainloop.db.postgres import MIGRATION_SQL
from mainloop.services import github_creation as creation
from mainloop.services.github_auth import GitHubRefusal
from mainloop.services.github_repo import parse_github_repo
from pydantic import ValidationError
from tests.runtime.github_app_fake import app_settings, app_transport
from tests.runtime.test_open_pull_request import ARGS
from tests.runtime.test_postgres_ledger import PostgresTestCase

from models.agent_tools import OpenPullRequest


def percent(value):
    return "".join(f"%{byte:02X}" for byte in value.encode())


def b64(value):
    return base64.b64encode(value.encode()).decode()


# All 25 reviewer reflections, using only synthetic credentials.
CURRENT = "secret-fixture"
OPAQUE = "outside-fixture-credential"
GHS = "ghs_" + "A" * 36
PAT = "github_pat_" + "B" * 36
JWT = "eyJhbGciOiJIUzI1NiJ9.e30.QQ"
URL = f"https://x-access-token:{CURRENT}@github.com/owner/repo"
CREDENTIAL_REFLECTIONS = [
    ("raw_current_installation", CURRENT, CURRENT),
    ("raw_bearer", f"Bearer {OPAQUE}", OPAQUE),
    ("raw_token_equals", f"token={OPAQUE}", OPAQUE),
    ("authorization_bearer", f"Authorization: Bearer {OPAQUE}", OPAQUE),
    ("token_bearer_scheme", f"token: Bearer {OPAQUE}", OPAQUE),
    ("raw_jwt", JWT, JWT),
    ("embedded_ghs", f"prefix_{GHS}", GHS),
    ("embedded_github_pat", f"prefix_{PAT}", PAT),
    ("raw_x_access_token_url_current", URL, CURRENT),
    ("raw_x_access_token_url_ghs", URL.replace(CURRENT, GHS), GHS),
    ("percent_current_bearer", f"Bearer {percent(CURRENT)}", percent(CURRENT)),
    ("percent_current_token", f"token={percent(CURRENT)}", percent(CURRENT)),
    ("percent_current_standalone", percent(CURRENT), percent(CURRENT)),
    ("percent_ghs_prefix", percent("ghs_") + "A" * 36, percent("ghs_") + "A" * 36),
    ("percent_jwt_dots", JWT.replace(".", "%2E"), JWT.replace(".", "%2E")),
    (
        "percent_x_access_token_password",
        URL.replace(CURRENT, percent(CURRENT)),
        percent(CURRENT),
    ),
    ("percent_whole_x_access_token_url", percent(URL), percent(CURRENT)),
    ("base64_current_basic", f"Basic {b64(CURRENT)}", b64(CURRENT)),
    ("base64_current_bearer", f"Bearer {b64(CURRENT)}", b64(CURRENT)),
    ("base64_current_standalone", b64(CURRENT), b64(CURRENT)),
    ("base64_ghs_basic", f"Basic {b64(GHS)}", b64(GHS)),
    ("base64_ghs_standalone", b64(GHS), b64(GHS)),
    ("base64_jwt_standalone", b64(JWT), b64(JWT)),
    ("base64_whole_url_basic", f"Basic {b64(URL)}", b64(URL)),
    ("base64_whole_url_standalone", b64(URL), b64(URL)),
]


class RefusalHTTPTests(unittest.IsolatedAsyncioTestCase):
    async def create(self, response, *, timeout=15):
        with app_settings(), patch.object(creation, "REQUEST_TIMEOUT_SECONDS", timeout):
            async with creation.GitHubCreationClient(
                "owner/repo", transport=app_transport(lambda _: response)
            ) as client:
                return await client.create(
                    "owner/repo", OpenPullRequest.model_validate(ARGS), "trunk"
                )

    async def test_diagnostics_are_selected_bounded_and_secret_free(self):
        details = {
            "message": "Validation Failed\nBearer secret-fixture github_pat_fake123 "
            + "x" * 1000,
            "errors": [
                {
                    "resource": "PullRequest",
                    "field": "head" + "x" * 100,
                    "code": "invalid",
                    "message": "DO-NOT-COPY-ERROR-MESSAGE",
                    "value": "DO-NOT-COPY-VALUE",
                    "token": "DO-NOT-COPY-TOKEN",
                }
            ]
            * 20,
            "headers": {"Authorization": "DO-NOT-COPY-HEADER"},
        }
        with self.assertRaises(GitHubRefusal) as caught:
            await self.create(httpx.Response(422, json=details))
        error = caught.exception
        self.assertEqual(error.status, 422)
        self.assertIsNone(error.message)
        self.assertEqual(len(error.errors), 10)
        self.assertEqual(
            error.errors,
            [{"resource": "PullRequest", "field": None, "code": "invalid"}] * 10,
        )
        with self.assertLogs(creation.logger, level="WARNING") as logs:
            result = creation._refused(error)
        combined = json.dumps(result) + str(logs.output)
        self.assertNotIn("github_message", result)
        for secret in (
            "secret-fixture",
            "github_pat_fake123",
            "DO-NOT-COPY",
            "\nBearer",
        ):
            self.assertNotIn(secret, combined)
        self.assertNotIn("Validation Failed", str(logs.output))
        self.assertEqual(str(error), "")
        self.assertFalse(hasattr(error, "request"))
        self.assertFalse(hasattr(error, "response"))

    async def test_reflected_current_installation_token_is_omitted(self):
        with self.assertRaises(GitHubRefusal) as caught:
            await self.create(
                httpx.Response(403, json={"message": "Rejected secret-fixture"})
            )
        self.assertIsNone(caught.exception.message)
        with self.assertLogs(creation.logger, level="WARNING") as logs:
            result = creation._refused(caught.exception)
        self.assertNotIn("github_message", result)
        for output in (json.dumps(result), str(logs.output)):
            self.assertNotIn(CURRENT, output)

    async def test_complete_authorization_values_are_removed_from_results_and_logs(
        self,
    ):
        credential = "cmV2aWV3LWZpeHR1cmU="  # Synthetic, never a live credential.
        for text in (
            f"Authorization: Basic {credential}",
            f"Authorization: Bearer {credential}",
            f"authorization=basic {credential}",
            f'"Authorization": "Basic {credential}"',
            f'Authorization: Basic "{credential}"',
            f'Authorization: Digest username="{credential}", response="{credential}"',
            f"Proxy-Authorization: Basic {credential}",
            f"Authorization: OAuth {credential}",
            f"Authorization:\nBasic\t{credential}",
            f"Basic {credential}",
            f"Bearer {credential}",
        ):
            with self.subTest(text=text):
                with self.assertRaises(GitHubRefusal) as caught:
                    await self.create(
                        httpx.Response(
                            422,
                            json={
                                "message": f"Validation Failed: {text}",
                                "errors": [
                                    {key: text for key in ("resource", "field", "code")}
                                ],
                            },
                        )
                    )
                with self.assertLogs(creation.logger, level="WARNING") as logs:
                    result = creation._refused(caught.exception)
                self.assertNotIn("github_message", result)
                self.assertEqual(
                    result["github_errors"],
                    [{"resource": None, "field": None, "code": None}],
                )
                for output in (json.dumps(result), str(logs.output)):
                    self.assertNotIn(credential, output)
                    self.assertNotIn("Basic", output)
                    self.assertNotIn("Bearer", output)

    async def test_embedded_github_credentials_are_removed_from_results_and_logs(self):
        for kind in ("ghp_", "gho_", "ghs_", "ghu_", "ghr_", "github_pat_"):
            credential = kind + "A" * 36  # Synthetic GitHub credential shapes.
            for prefix in ("", "_", "a", "prefix_", "é"):
                with self.subTest(kind=kind, prefix=prefix):
                    text = prefix + credential
                    with self.assertRaises(GitHubRefusal) as caught:
                        await self.create(
                            httpx.Response(
                                422,
                                json={
                                    "message": f"Invalid branch {text}",
                                    "errors": [{"code": text}],
                                },
                            )
                        )
                    with self.assertLogs(creation.logger, level="WARNING") as logs:
                        result = creation._refused(caught.exception)
                    self.assertEqual(
                        result["github_errors"],
                        [{"resource": None, "field": None, "code": None}],
                    )
                    self.assertNotIn("github_message", result)
                    for output in (json.dumps(result), str(logs.output)):
                        self.assertNotIn(credential, output)
                        self.assertNotIn(credential[:16], output)

    async def test_all_reviewer_credential_reflections_are_omitted(self):
        for name, reflection, forbidden in CREDENTIAL_REFLECTIONS:
            with self.subTest(case=name):
                with self.assertRaises(GitHubRefusal) as caught:
                    await self.create(
                        httpx.Response(
                            422,
                            json={
                                "message": f"Rejected {reflection}",
                                "errors": [
                                    {
                                        "resource": reflection,
                                        "field": reflection,
                                        "code": reflection,
                                        "message": reflection,
                                    }
                                ],
                                "headers": {"Authorization": "DO-NOT-COPY-HEADER"},
                                "other": "DO-NOT-COPY-ARBITRARY-FIELD",
                            },
                        )
                    )
                with self.assertLogs(creation.logger, level="WARNING") as logs:
                    result = creation._refused(caught.exception)
                self.assertEqual(result["state"], "refused")
                self.assertEqual(result["http_status"], 422)
                self.assertNotIn("github_message", result)
                self.assertNotIn("github_hints", result)
                self.assertEqual(
                    result["github_errors"],
                    [{"resource": None, "field": None, "code": None}],
                )
                for output in (json.dumps(result), str(logs.output)):
                    self.assertNotIn(forbidden, output)
                    self.assertNotIn("DO-NOT-COPY", output)
                    decoded = [unquote(output)]
                    for encoded in re.findall(r"[A-Za-z0-9+/]{8,}={0,2}", output):
                        # Also check truncated base64, which can retain a complete
                        # password despite losing its encoded URL's suffix.
                        if len(encoded) % 4 == 1:
                            encoded = encoded[:-1]
                        try:
                            decoded.append(
                                base64.b64decode(
                                    encoded + "=" * (-len(encoded) % 4)
                                ).decode(errors="ignore")
                            )
                        except (ValueError, binascii.Error):
                            pass
                    for secret in (CURRENT, OPAQUE, GHS, PAT, JWT):
                        self.assertNotIn(secret, output)
                        for recovered in decoded:
                            self.assertNotIn(secret, recovered)

    async def test_normal_validation_failure_retains_fixed_diagnostics_and_hint(self):
        with self.assertRaises(GitHubRefusal) as caught:
            await self.create(
                httpx.Response(
                    422,
                    json={
                        "message": "Validation Failed",
                        "errors": [
                            {
                                "resource": "PullRequest",
                                "code": "custom",
                                "message": f"No commits between main and {CURRENT}",
                            }
                        ],
                    },
                )
            )
        with self.assertLogs(creation.logger, level="WARNING") as logs:
            result = creation._refused(caught.exception)
        self.assertEqual(result["github_message"], "Validation Failed")
        self.assertEqual(
            result["github_errors"],
            [{"resource": "PullRequest", "field": None, "code": "custom"}],
        )
        self.assertEqual(result["github_hints"], ["no_commits"])
        for output in (json.dumps(result), str(logs.output)):
            for allowed in ("Validation Failed", "PullRequest", "custom", "no_commits"):
                self.assertIn(allowed, output)
            self.assertNotIn("No commits between", output)
            self.assertNotIn(CURRENT, output)

    async def test_messages_names_and_codes_require_exact_known_values(self):
        for message in (
            "Validation Failed",
            "Not Found",
            "Resource not accessible by integration",
            "Must have admin rights to Repository.",
            "Bad credentials",
        ):
            with self.subTest(message=message):
                with self.assertRaises(GitHubRefusal) as caught:
                    await self.create(httpx.Response(422, json={"message": message}))
                self.assertEqual(caught.exception.message, message)
        errors = [
            {"resource": "Repository", "field": "head.sha", "code": code}
            for code in (
                "missing",
                "missing_field",
                "invalid",
                "already_exists",
                "unprocessable",
                "custom",
            )
        ]
        errors.append({"resource": "Unknown", "field": "a.b", "code": "secret"})
        errors.append(
            {"resource": "PullRequest\n", "field": "head[]", "code": "invalid "}
        )
        with self.assertRaises(GitHubRefusal) as caught:
            await self.create(
                httpx.Response(
                    422, json={"message": "Validation Failed ", "errors": errors}
                )
            )
        self.assertIsNone(caught.exception.message)
        self.assertEqual(caught.exception.errors[:6], errors[:6])
        self.assertEqual(
            caught.exception.errors[6:],
            [{"resource": None, "field": None, "code": None}] * 2,
        )

    async def test_hints_never_echo_error_messages_and_are_bounded(self):
        text = (
            f"No commits between main and {CURRENT}. A pull request already exists. "
            "Draft pull requests are not supported."
        )
        with self.assertRaises(GitHubRefusal) as caught:
            await self.create(
                httpx.Response(
                    422,
                    json={
                        "message": text,
                        "errors": [
                            {"field": "head", "code": "invalid", "message": text}
                        ]
                        * 20,
                    },
                )
            )
        with self.assertLogs(creation.logger, level="WARNING") as logs:
            result = creation._refused(caught.exception)
        self.assertNotIn("github_message", result)
        self.assertEqual(len(result["github_errors"]), 10)
        self.assertEqual(
            result["github_hints"],
            ["no_commits", "already_exists", "draft_unsupported", "invalid_head"],
        )
        for output in (json.dumps(result), str(logs.output)):
            self.assertNotIn(CURRENT, output)
            self.assertNotIn("No commits between", output)
            self.assertNotIn("A pull request already exists", output)
            self.assertNotIn("Draft pull requests are not supported", output)

    async def test_invalid_and_oversize_error_bodies_still_refuse(self):
        for response in (
            httpx.Response(422, content=b"not json"),
            httpx.Response(404, json=[{"message": "wrong shape"}]),
            httpx.Response(400, content=b"x" * 2_000_001),
            httpx.Response(403, json={"message": 123, "errors": [None, "custom"]}),
        ):
            with self.subTest(status=response.status_code):
                with self.assertRaises(GitHubRefusal) as caught:
                    await self.create(response)
                self.assertEqual(caught.exception.status, response.status_code)
                self.assertIsNone(caught.exception.message)
                self.assertEqual(caught.exception.errors, [])

    async def test_body_deadline_keeps_observed_refusal(self):
        class SlowStream(httpx.AsyncByteStream):
            async def __aiter__(self):
                await asyncio.sleep(0.05)
                yield b"{}"

        with self.assertRaises(GitHubRefusal) as caught:
            await self.create(httpx.Response(422, stream=SlowStream()), timeout=0.01)
        self.assertEqual(caught.exception.status, 422)

    async def test_body_transport_failure_keeps_observed_refusal(self):
        class BrokenStream(httpx.AsyncByteStream):
            async def __aiter__(self):
                yield b"{"
                raise httpx.ReadError("Bearer secret-fixture")

        with self.assertRaises(GitHubRefusal) as caught:
            await self.create(httpx.Response(422, stream=BrokenStream()))
        self.assertEqual(caught.exception.status, 422)

    async def test_invalid_success_bodies_are_not_definite_refusals(self):
        for response in (
            httpx.Response(201, content=b"not json"),
            httpx.Response(201, json={}),
            httpx.Response(302, headers={"Location": "https://evil.invalid"}),
        ):
            with self.subTest(status=response.status_code):
                with self.assertRaises(
                    (creation.GitHubError, ValidationError)
                ) as caught:
                    await self.create(response)
                self.assertNotIsInstance(caught.exception, GitHubRefusal)

    async def test_mint_422_never_becomes_product_refusal(self):
        requests = []

        def handler(request):
            requests.append(request)
            if request.url.path.endswith("/installation"):
                return httpx.Response(200, json={"id": 456})
            return httpx.Response(422, json={"message": "Auth failure"})

        with app_settings():
            async with creation.GitHubCreationClient(
                "owner/repo", transport=httpx.MockTransport(handler)
            ) as client:
                with self.assertRaises(creation.GitHubError) as caught:
                    await client.create(
                        "owner/repo", OpenPullRequest.model_validate(ARGS), "trunk"
                    )
        self.assertNotIsInstance(caught.exception, GitHubRefusal)
        self.assertEqual(len(requests), 2)

    async def test_auth_refusal_diagnostics_stay_opaque(self):
        credential = "cmV2aWV3LWZpeHR1cmU="
        for status in (400, 401, 403, 404, 422, 429, 500):
            requests = []

            def handler(request, *, status=status, requests=requests):
                requests.append(request)
                if request.url.path.endswith("/installation"):
                    return httpx.Response(200, json={"id": 456})
                return httpx.Response(
                    status,
                    json={
                        "message": f"Authorization: Basic {credential}",
                        "errors": [{"code": "prefix_ghp_" + "A" * 36}],
                    },
                )

            with self.subTest(status=status), app_settings():
                async with creation.GitHubCreationClient(
                    "owner/repo", transport=httpx.MockTransport(handler)
                ) as client:
                    with self.assertNoLogs(creation.logger, level="WARNING"):
                        with self.assertRaises(creation.GitHubError) as caught:
                            await client.create(
                                "owner/repo",
                                OpenPullRequest.model_validate(ARGS),
                                "trunk",
                            )
                self.assertNotIsInstance(caught.exception, GitHubRefusal)
                self.assertEqual(str(caught.exception), "")
                self.assertEqual(len(requests), 2)


class RefusalMigrationTests(PostgresTestCase):
    async def test_legacy_refusal_recovery_preserves_old_ids_and_releases_tuple(self):
        project = await db.get_or_create_project(
            self.user, parse_github_repo("owner/repo")
        )
        claim = {
            "user_id": self.user,
            "project_id": project.id,
            "request_id": "legacy-422",
            "payload_hash": "old-payload",
            "repo_id": 123,
            "head": "feature/fix",
            "base": "trunk",
            "expected_sha": "a" * 40,
        }
        # Recreate exactly the previous constraints before seeding a stuck intent.
        await self.pool.execute(
            """DROP INDEX idx_pr_creations_active_tuple;
               ALTER TABLE pr_creations DROP CONSTRAINT pr_creations_state_v2_check;
               ALTER TABLE pr_creations ADD CONSTRAINT pr_creations_state_check
                   CHECK (state IN ('uncertain','created'));
               ALTER TABLE pr_creations ADD CONSTRAINT pr_creations_user_id_repo_id_head_base_key
                   UNIQUE(user_id,repo_id,head,base);"""
        )
        await self.pool.execute(
            """INSERT INTO pr_creations
               (id,user_id,project_id,repo_id,head,base,expected_sha,payload_hash)
               VALUES ('legacy-intent',$1,$2,123,'feature/fix','trunk',$3,'old-payload');
               """,
            self.user,
            project.id,
            "a" * 40,
        )
        await self.pool.execute(
            "INSERT INTO pr_creation_requests VALUES ($1,'legacy-422','old-payload','legacy-intent')",
            self.user,
        )
        await self.pool.execute(MIGRATION_SQL)
        await self.pool.execute(MIGRATION_SQL)
        old, creator = await db.claim_pr_creation(**claim)
        self.assertFalse(creator)
        self.assertEqual(old["state"], "uncertain")
        # Migration and absence alone never manufacture proof of a failure.
        still_old, creator = await db.claim_pr_creation(
            **{**claim, "request_id": "alias"}
        )
        self.assertFalse(creator)
        self.assertEqual(still_old["id"], old["id"])
        refusal = GitHubRefusal()
        refusal.status = (
            422  # Trusted historical POST evidence supplied by the operator.
        )
        with self.assertLogs(creation.logger, level="WARNING"):
            result = creation._refused(refusal)
        await db.refuse_pr_creation(old["id"], result)
        await db.refuse_pr_creation(old["id"], result)
        stale = await db.finish_pr_creation(old["id"], {"state": "created"})
        self.assertEqual(stale["state"], "refused")
        self.assertEqual(json.loads(stale["result"]), result)
        for request_id in ("legacy-422", "alias"):
            saved, creator = await db.claim_pr_creation(
                **{**claim, "request_id": request_id}
            )
            self.assertFalse(creator)
            self.assertEqual(saved["state"], "refused")
            self.assertEqual(json.loads(saved["result"]), result)
        new_claim = {**claim, "request_id": "corrected", "payload_hash": "new-payload"}
        contenders = await asyncio.gather(
            *(db.claim_pr_creation(**new_claim) for _ in range(12))
        )
        self.assertEqual(sum(creator for _, creator in contenders), 1)
        replacement = contenders[0][0]
        self.assertNotEqual(replacement["id"], old["id"])
        created = await db.finish_pr_creation(replacement["id"], {"state": "created"})
        self.assertEqual(created["state"], "created")
        lost = await db.refuse_pr_creation(replacement["id"], result)
        self.assertEqual(lost, created)
        changed = await db.finish_pr_creation(
            replacement["id"], {"state": "created", "pr_number": 99}
        )
        self.assertEqual(changed, created)
        await self.pool.execute(MIGRATION_SQL)
        saved = await db.get_pr_creation_request(self.user, "legacy-422", "old-payload")
        self.assertEqual(saved["state"], "refused")
