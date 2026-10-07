"""Sanitized protocol/configuration fixtures; no provider or gateway calls."""

import json
import unittest
from pathlib import Path

from mainloop.runtime.hitl_correlation import (
    build_decision_receipt,
    canonical_operation,
    validate_receipt,
)
from pydantic import ValidationError

from models.hitl import (
    HITL_EXTENSION,
    AskUserRequest,
    AskUserResponse,
    ContinuationIdentity,
    HITLMessageMetadata,
    NestedHITLRequest,
    TaskIdentity,
    TemplateMergeConfiguration,
    ToolApprovalRequest,
    ToolApprovalResponse,
    VerifiedAssociation,
    normalized_hash,
)
from models.merge_policy import (
    ChangedPath,
    MergePolicy,
    approval_required,
    protected_matches,
)


def task(session="leaf", task_id="task"):
    return TaskIdentity(
        gateway="fixture-gateway",
        endpoint="http://fixture/agents/claude",
        runtime_session_id=session,
        context_id=session,
        task_id=task_id,
    )


def configuration(provider="claude", binding="binding", session="leaf"):
    del binding, session
    return TemplateMergeConfiguration(
        template_name="claude-workspace-template",
        provider=provider,
        compiled_alias="mainloop-merge-approval",
        endpoint="http://mainloop-mcp/merge-approval",
        tool="merge_pull_request_with_approval",
        require_approval=True,
    )


def request(name=None, request_id="invoke-1"):
    return ToolApprovalRequest(
        type="tool_approval_request",
        tools=[
            {
                "id": "pending-1",
                "call_id": "native-call",
                "name": name or configuration().public_name(),
                "args": {"proposal_id": "proposal-1", "request_id": request_id},
            }
        ],
    )


def response(approved=True):
    return ToolApprovalResponse.model_validate_json(
        '{"type":"tool_approval_response","approvals":[{"id":"pending-1","approved":'
        + ("true" if approved else "false")
        + "}]}"
    )


def receipt(
    *,
    req=None,
    config=None,
    nested=False,
    action="action",
    approved=True,
    leaf_task=None,
    associations=None,
):
    req = req or request()
    leaf_task = leaf_task or task()
    outer_task = task("outer", "parent-task") if nested else leaf_task
    outer_req = req
    if nested:
        outer_req = ToolApprovalRequest(
            type="tool_approval_request",
            tools=[
                {
                    "id": "parent-pending",
                    "call_id": "parent-call",
                    "name": "delegate",
                    "args": {},
                }
            ],
            nested=NestedHITLRequest(
                task_id=leaf_task.task_id,
                context_id=leaf_task.context_id,
                tools=req.tools,
            ),
        )
    outer = ContinuationIdentity(
        **outer_task.model_dump(),
        status_message_id="status",
        request_hash=normalized_hash(outer_req.model_dump(mode="json")),
    )
    if associations is None:
        associations = (
            (
                VerifiedAssociation(
                    owner_id="owner",
                    outer=outer_task,
                    leaf=leaf_task,
                    evidence_source="gateway_continuation",
                    evidence_reference="fixture://delegation",
                ),
            )
            if nested
            else ()
        )
    selected_config = config if config is not None else configuration()
    return build_decision_receipt(
        action_id=action,
        owner_id="owner",
        outbound_message_id=f"message-{action}",
        outer=outer,
        request=outer_req,
        leaf_task=leaf_task,
        leaf_request=req,
        leaf_binding_id="binding",
        response=response(approved),
        configuration=selected_config,
        mapping_evidence=(selected_config.evidence() if selected_config else None),
        associations=associations,
        validate_proposal=lambda key, approved: None,
    )


class CorrelationTests(unittest.TestCase):
    def test_exact_provider_names_and_unknown_mapping(self):
        for provider in ("claude", "codex"):
            config = configuration(provider)
            result = receipt(req=request(config.public_name()), config=config)
            self.assertEqual(result.calls[0].mapping, config)
            validate_receipt(result)
            for name in (
                "merge_pull_request_with_approval",
                "other.merge_pull_request_with_approval",
                "mcp__other__merge_pull_request_with_approval",
                config.public_name() + "extra",
                config.public_name().replace(
                    "merge_pull_request_with_approval", "different_tool"
                ),
            ):
                self.assertIsNone(
                    receipt(req=request(name), config=config).calls[0].merge_key
                )
            self.assertIsNone(canonical_operation(config.public_name(), None))
            other_provider = configuration(
                "codex" if provider == "claude" else "claude"
            )
            self.assertIsNone(canonical_operation(config.public_name(), other_provider))
            changed_alias = config.model_copy(update={"compiled_alias": "other"})
            self.assertIsNone(canonical_operation(config.public_name(), changed_alias))

    def test_hint_and_metadata_cannot_mint_consent(self):
        req = request("mcp__other__merge_pull_request_with_approval")
        req.hint = configuration().public_name()
        req.__pydantic_extra__["canonical_operation"] = (
            "mainloop.merge_pull_request_with_approval.v1"
        )
        meta = HITLMessageMetadata(
            metadata={
                HITL_EXTENSION: req.model_dump(mode="json"),
                "unknown": {"approved": True},
            },
            extensions=[HITL_EXTENSION, "unknown"],
        )
        self.assertIsNone(receipt(req=meta.request()).calls[0].merge_key)
        self.assertEqual(meta.model_dump()["metadata"]["unknown"], {"approved": True})
        with self.assertRaises(ValueError):
            HITLMessageMetadata(metadata=meta.metadata).request()

    def test_nested_leaf_uses_child_ids_and_requires_trusted_association(self):
        direct, nested = receipt(), receipt(nested=True)
        self.assertEqual(direct.calls[0].leaf.key(), nested.calls[0].leaf.key())
        self.assertEqual(nested.calls[0].leaf.runtime_session_id, "leaf")
        self.assertEqual(nested.outer.runtime_session_id, "outer")
        self.assertEqual(nested.response.approvals[0].id, "pending-1")
        validate_receipt(nested)
        with self.assertRaisesRegex(ValueError, "mapping unavailable"):
            receipt(nested=True, associations=())
        # A Mainloop parent_session_id is deliberately not an input to direct correlation.
        self.assertEqual(
            direct.outer.runtime_session_id, direct.calls[0].leaf.runtime_session_id
        )

    def test_reused_call_ids_are_scoped_to_tasks(self):
        first, second = receipt(), receipt(leaf_task=task(task_id="another-task"))
        self.assertEqual(first.calls[0].call_id, second.calls[0].call_id)
        self.assertNotEqual(first.calls[0].leaf.key(), second.calls[0].leaf.key())

    def test_configuration_contains_no_session_or_revision_identity(self):
        fields = set(type(configuration()).model_fields)
        self.assertEqual(
            fields,
            {
                "template_name",
                "provider",
                "compiled_alias",
                "endpoint",
                "tool",
                "require_approval",
                "operation",
            },
        )

    def test_legacy_immutable_receipt_shape_remains_readable(self):
        saved = json.loads(receipt().model_dump_json())
        legacy_mapping = {
            "provider": "claude",
            "prepared_revision": "pinned-revision",
            "compiled_alias": "mainloop-merge-approval",
            "remote_server_id": "namespace/protected",
            "endpoint": "http://mainloop-mcp/merge-approval",
            "tool": "merge_pull_request_with_approval",
            "require_approval": True,
            "operation": "mainloop.merge_pull_request_with_approval.v1",
        }
        saved["calls"][0]["configuration"] = {
            "owner_id": "owner",
            "binding_id": "binding",
            "runtime_session_id": "leaf",
            "provider": "claude",
            "prepared_revision": "pinned-revision",
            "evidence_reference": "fixture://verified-config",
            "mappings": [legacy_mapping],
        }
        saved["calls"][0]["mapping"] = legacy_mapping
        saved["calls"][0]["mapping_evidence"] = None
        old_receipt = type(receipt()).model_validate_json(json.dumps(saved))
        validate_receipt(old_receipt)

    def test_arguments_and_response_completeness(self):
        req = request()
        req.tools[0].args["extra"] = "no"
        with self.assertRaises(ValidationError):
            receipt(req=req)
        result = receipt()
        bad = result.model_copy(
            update={
                "response": ToolApprovalResponse.model_validate_json(
                    '{"type":"tool_approval_response","approvals":[{"id":"other","approved":true}]}'
                )
            }
        )
        with self.assertRaises(ValueError):
            validate_receipt(bad)
        with self.assertRaises(ValueError):
            normalized_hash({"huge": "x" * 131073})
        with self.assertRaises(ValidationError):
            ToolApprovalRequest(
                type="tool_approval_request", tools=[request().tools[0]] * 2
            )

    def test_native_free_text_direct_and_propagated_wire_correlation(self):
        folder = Path(__file__).parent / "fixtures" / "kagent"
        direct = HITLMessageMetadata.model_validate_json(
            (folder / "hitl-free-text-direct.json").read_text()
        ).request()
        self.assertIsNone(direct.questions[0].choices)
        for filename in (
            "hitl-free-text-direct.json",
            "hitl-free-text-propagated.json",
        ):
            envelope = HITLMessageMetadata.model_validate_json(
                (folder / filename).read_text()
            )
            req = envelope.request()
            self.assertIsNone(req.model_dump(mode="json")["questions"][0]["choices"])
            outer_task = task("outer", "parent-task") if req.nested else task()
            outer = ContinuationIdentity(
                **outer_task.model_dump(),
                status_message_id="status",
                request_hash=normalized_hash(req.model_dump(mode="json")),
            )
            associations = (
                (
                    VerifiedAssociation(
                        owner_id="owner",
                        outer=outer_task,
                        leaf=task(),
                        evidence_source="gateway_continuation",
                        evidence_reference="fixture://delegation",
                    ),
                )
                if req.nested
                else ()
            )
            answer = AskUserResponse.model_validate_json(
                '{"type":"ask_user_response","id":"ask-child","answers":[{"answer":["feature/free-text"]}]}'
            )
            result = build_decision_receipt(
                action_id="a",
                owner_id="owner",
                outbound_message_id="m",
                outer=outer,
                request=req,
                leaf_task=task(),
                leaf_request=direct,
                leaf_binding_id=None,
                response=answer,
                associations=associations,
            )
            validate_receipt(result)
            self.assertEqual(result.response.id, "ask-child")
            self.assertEqual(
                result.outer.runtime_session_id, outer_task.runtime_session_id
            )
            self.assertIsNone(result.calls[0].merge_key)
        raw = direct.model_dump(mode="json")
        for invalid in ("not a list", 4, {}, ["x"] * 101, ["x" * 4097]):
            raw["questions"][0]["choices"] = invalid
            with self.assertRaises(ValidationError):
                AskUserRequest.model_validate(raw)
        raw["questions"][0]["choices"] = None
        raw["oversized"] = "x" * 131073
        with self.assertRaises(ValueError):
            AskUserRequest.model_validate(raw)

    def test_approval_with_rejection_reason_fails_model_and_builder(self):
        for reason in ("Do not merge", " "):
            wire = {
                "type": "tool_approval_response",
                "approvals": [
                    {"id": "pending-1", "approved": True, "rejection_reason": reason}
                ],
            }
            with self.assertRaises(ValidationError):
                ToolApprovalResponse.model_validate_json(json.dumps(wire))
        for approved, reason in ((True, ""), (True, None), (False, "Do not merge")):
            ToolApprovalResponse.model_validate_json(
                json.dumps(
                    {
                        "type": "tool_approval_response",
                        "approvals": [
                            {
                                "id": "pending-1",
                                "approved": approved,
                                "rejection_reason": reason,
                            }
                        ],
                    }
                )
            )
        value = receipt()
        invalid = value.response.model_copy(
            update={
                "approvals": (
                    value.response.approvals[0].model_copy(
                        update={"rejection_reason": "Do not merge"}
                    ),
                )
            }
        )
        with self.assertRaises(ValidationError):
            build_decision_receipt(
                action_id="a",
                owner_id="owner",
                outbound_message_id="m",
                outer=value.outer,
                request=value.request,
                leaf_task=task(),
                leaf_request=value.request,
                leaf_binding_id="binding",
                response=invalid,
                configuration=configuration(),
                mapping_evidence=configuration().evidence(),
                validate_proposal=lambda key, approved: None,
            )
        with self.assertRaises(ValidationError):
            validate_receipt(value.model_copy(update={"response": invalid}))

    def test_standalone_questions_need_no_tool_binding(self):
        req = AskUserRequest(
            type="ask_user_request",
            id="question",
            questions=[
                {"question": "Which?", "choices": ["one", "two"], "multiple": False}
            ],
        )
        outer = ContinuationIdentity(
            **task().model_dump(),
            status_message_id="status",
            request_hash=normalized_hash(req.model_dump(mode="json")),
        )
        answer = AskUserResponse.model_validate_json(
            '{"type":"ask_user_response","id":"question","answers":[{"answer":["free text"]}]}'
        )
        result = build_decision_receipt(
            action_id="a",
            owner_id="owner",
            outbound_message_id="m",
            outer=outer,
            request=req,
            leaf_task=task(),
            leaf_request=req,
            leaf_binding_id=None,
            response=answer,
        )
        self.assertIsNone(result.calls[0].merge_key)
        validate_receipt(result)


class ProtectedPathsTests(unittest.TestCase):
    def test_fixed_globs_include_zero_directories_renames_deletions(self):
        files = [
            ChangedPath(filename="migrations/001.sql", status="removed"),
            ChangedPath(
                filename="safe/file", status="renamed", previous_filename="k8s/app.yaml"
            ),
            ChangedPath(filename="src/db/migrations/file", status="modified"),
            ChangedPath(filename=".github/workflows/ci.yaml", status="added"),
            ChangedPath(filename="K8s/unprotected", status="added"),
        ]
        self.assertEqual(
            protected_matches(files, complete=True),
            (
                ".github/workflows/ci.yaml",
                "k8s/app.yaml",
                "migrations/001.sql",
                "src/db/migrations/file",
            ),
        )
        self.assertTrue(approval_required(MergePolicy.AUTO, files, complete=True))
        self.assertFalse(approval_required(MergePolicy.AUTO, files[-1:], complete=True))
        self.assertTrue(approval_required(MergePolicy.APPROVAL, [], complete=True))
        for candidate_files, complete in (
            (files, False),
            ([ChangedPath(filename="safe", status="renamed")], True),
            ([ChangedPath(filename="../k8s/a", status="added")], True),
        ):
            with self.assertRaises(ValueError):
                protected_matches(candidate_files, complete=complete)
