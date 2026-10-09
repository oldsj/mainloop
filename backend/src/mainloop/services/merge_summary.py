"""Deterministic, bounded presentation snapshots for immutable merge proposals."""

import hashlib
import json
import re

from models.hitl import TemplateMappingEvidence
from models.merge_policy import matched_protected_globs

MAX_DESCRIPTION_LENGTH = 16 * 1024
SUMMARY_DESCRIPTION_LENGTH = 320
SUMMARY_PATH_LIMIT = 5
PENDING_CHECK_STATES = {"queued", "in_progress", "pending", "waiting", "requested"}


def canonical_digest(value) -> str:
    encoded = json.dumps(
        value,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
        allow_nan=False,
    )
    return hashlib.sha256(encoded.encode("utf-8")).hexdigest()


def _ci_summary(ci):
    if not isinstance(ci, dict):
        return None, None
    suites = ci.get("suites")
    checks = ci.get("checks")
    statuses = ci.get("statuses")
    if not all(isinstance(items, list) for items in (suites, checks, statuses)):
        return None, None
    if not all(
        all(isinstance(item, dict) for item in items)
        for items in (suites, checks, statuses)
    ):
        return None, None
    required = ci.get("required")
    if not isinstance(required, list) or type(ci.get("green")) is not bool:
        return None, None
    inventory = {
        "green": ci.get("green"),
        "pending": ci.get("pending"),
        "suites": suites,
        "checks": checks,
        "statuses": statuses,
        "required": required,
        "captured_at": ci.get("captured_at"),
        "complete": ci.get("complete"),
    }
    ignored = []
    if "ignored_suites" in ci:
        ignored = ci["ignored_suites"]
        if not isinstance(ignored, list) or not all(
            isinstance(item, dict)
            and type(item.get("id")) is int
            and isinstance(item.get("reason"), str)
            and bool(item["reason"])
            and item.get("status") == "queued"
            and item.get("conclusion") is None
            and {k: v for k, v in item.items() if k != "reason"} in suites
            for item in ignored
        ):
            return None, None
        ignored_ids = {item["id"] for item in ignored}
        if len(ignored_ids) != len(ignored):
            return None, None
        # Preserve old immutable proposal digests when this field is absent.
        inventory["ignored_suites"] = ignored
        suites = [item for item in suites if item.get("id") not in ignored_ids]
    total = len(suites) + len(checks) + len(statuses)
    passed = sum(
        item.get("status") == "completed" and item.get("conclusion") == "success"
        for item in suites + checks
    ) + sum(item.get("state") == "success" for item in statuses)
    failed = sum(
        item.get("status") not in PENDING_CHECK_STATES
        and not (
            item.get("status") == "completed" and item.get("conclusion") == "success"
        )
        for item in suites + checks
    ) + sum(item.get("state") not in ("pending", "success") for item in statuses)
    pending = sum(
        item.get("status") in PENDING_CHECK_STATES for item in suites + checks
    ) + sum(item.get("state") == "pending" for item in statuses)
    summary = {
        "complete": ci.get("complete") is True
        and isinstance(ci.get("captured_at"), str)
        and bool(ci.get("captured_at")),
        "captured_at": ci.get("captured_at"),
        "result_count": total,
        "passed_count": passed,
        "failed_count": failed,
        "pending_count": pending,
        "green_at_preparation": ci.get("green") is True,
        "inventory_digest": canonical_digest(inventory),
    }
    if "ignored_suites" in ci:
        summary["ignored_suite_count"] = len(ignored)
    return summary, inventory


def build_summary(facts: dict, proposal_id: str) -> tuple[dict, str]:
    """Return the display snapshot and its digest from proposal-captured facts."""
    mapping_evidence = facts.get("mapping_evidence")
    mapping_required = facts.get("route") == "approval"
    mapping_valid = False
    if isinstance(mapping_evidence, dict):
        try:
            mapping_evidence = TemplateMappingEvidence.model_validate(mapping_evidence)
            mapping_evidence = mapping_evidence.model_dump(mode="json")
            mapping_valid = True
        except ValueError:
            mapping_evidence = None
    title = facts.get("title")
    description = facts.get("description")
    files = facts.get("files")
    changed_files = facts.get("changed_files_count")
    additions = facts.get("additions")
    deletions = facts.get("deletions")
    files_complete = (
        isinstance(files, list)
        and isinstance(changed_files, int)
        and not isinstance(changed_files, bool)
        and len(files) == changed_files
        and changed_files <= 3000
        and isinstance(facts.get("files_digest"), str)
        and re.fullmatch(r"[0-9a-f]{64}", facts["files_digest"]) is not None
        and isinstance(facts.get("description_digest"), str)
        and re.fullmatch(r"[0-9a-f]{64}", facts["description_digest"]) is not None
        and isinstance(additions, int)
        and not isinstance(additions, bool)
        and isinstance(deletions, int)
        and not isinstance(deletions, bool)
        and all(
            isinstance(item, dict)
            and isinstance(item.get("filename"), str)
            and bool(item.get("filename"))
            and isinstance(item.get("status"), str)
            and isinstance(item.get("additions"), int)
            and not isinstance(item.get("additions"), bool)
            and item["additions"] >= 0
            and isinstance(item.get("deletions"), int)
            and not isinstance(item.get("deletions"), bool)
            and item["deletions"] >= 0
            for item in files
        )
        and sum(item["additions"] for item in files) == additions
        and sum(item["deletions"] for item in files) == deletions
        and len({item["filename"] for item in files}) == len(files)
    )
    ci, _ = _ci_summary(facts.get("ci"))
    missing = []
    if not isinstance(title, str) or not title.strip():
        missing.append("GitHub PR title is unavailable")
    if not isinstance(description, str) or not description.strip():
        missing.append("GitHub PR description is unavailable")
    if not files_complete:
        missing.append("Complete changed-file details are unavailable")
    if ci is None or not ci["complete"]:
        missing.append("Complete recorded CI evidence is unavailable")
    if mapping_required and not mapping_valid:
        missing.append("Reviewed template mapping evidence is unavailable")

    file_rows = files if isinstance(files, list) else []
    protected_paths = facts.get("protected_matches")
    protected_paths = protected_paths if isinstance(protected_paths, list) else []
    protected_reasons = []
    for item in file_rows:
        if not isinstance(item, dict):
            continue
        for path in (item.get("filename"), item.get("previous_filename")):
            if not isinstance(path, str) or path not in protected_paths:
                continue
            protected_reasons.extend(
                {"glob": glob, "path": path} for glob in matched_protected_globs(path)
            )
    protected_reasons = sorted(
        {
            (reason["glob"], reason["path"]): reason for reason in protected_reasons
        }.values(),
        key=lambda reason: (reason["glob"], reason["path"]),
    )
    reasons = []
    if facts.get("policy") == "approval":
        reasons.append({"type": "project_policy"})
    reasons.extend({"type": "protected_path", **item} for item in protected_reasons)

    path_preview = []
    for item in file_rows:
        if not isinstance(item, dict) or not isinstance(item.get("filename"), str):
            continue
        old_path = item.get("previous_filename")
        display = f"{old_path} → {item['filename']}" if old_path else item["filename"]
        path_preview.append(display)
        if len(path_preview) == SUMMARY_PATH_LIMIT:
            break
    snapshot = {
        "version": 1,
        "proposal_id": proposal_id,
        "repository": facts.get("repository"),
        "pr_number": facts.get("pr_number"),
        "title": title,
        "description_excerpt": (
            description[:SUMMARY_DESCRIPTION_LENGTH]
            if isinstance(description, str)
            else ""
        ),
        "description_truncated": facts.get("description_truncated") is True
        or (
            isinstance(description, str)
            and len(description) > SUMMARY_DESCRIPTION_LENGTH
        ),
        "description_digest": facts.get("description_digest"),
        "description_length": facts.get("description_length"),
        "file_count": changed_files,
        "additions": additions,
        "deletions": deletions,
        "paths_digest": facts.get("files_digest"),
        "paths_preview": path_preview,
        "paths_preview_truncated": files_complete
        and len(file_rows) > len(path_preview),
        "protected_matches": sorted(set(protected_paths)),
        "approval_reasons": reasons,
        "policy": facts.get("policy"),
        "policy_version": facts.get("policy_version"),
        "globs_version": facts.get("globs_version"),
        "head": facts.get("head"),
        "head_sha": facts.get("head_sha"),
        "base": facts.get("base"),
        "base_sha": facts.get("base_sha"),
        "ci": ci,
        "mapping_evidence": mapping_evidence,
        "availability": "unavailable" if missing else "ready",
        "unavailable_reasons": missing,
    }
    return snapshot, canonical_digest(snapshot)


def store_summary(facts: dict, proposal_id: str) -> dict:
    summary, digest = build_summary(facts, proposal_id)
    return {**facts, "summary": summary, "summary_digest": digest}


def validate_reviewed_context(
    facts: dict,
    proposal_id: str,
    digest: str,
    stored_summary: dict | None,
    stored_digest: str | None,
) -> None:
    summary, current_digest = build_summary(facts, proposal_id)
    if (
        summary["availability"] != "ready"
        or stored_summary != summary
        or stored_digest != current_digest
        or digest != current_digest
    ):
        raise ValueError(
            "Merge approval context changed or is unavailable; refresh the request"
        )
