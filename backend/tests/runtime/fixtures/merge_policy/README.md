# Merge-policy acceptance fixtures

Synthetic, sanitized A2A task snapshots for Claude and Codex protected MCP approval
names. These are protocol fixtures, not recordings or proof of native pause/restore.
The tests substitute identities and the server-created proposal before observation.
Gateway inventory, suspended sessions, trusted configuration/association evidence,
and GitHub responses are fakes; observer, owner HTTP routes, correlation, merge
service and PostgreSQL persistence are real. No provider or cluster is contacted.

Run from the repository root with `MAINLOOP_TEST_DATABASE_URL` pointing to an
isolated PostgreSQL instance whose test role can create scratch databases:

```sh
export MERGE_UI_FIXTURE_PATH="$HOME/.cache/mainloop-merge-acceptance.json"
uv run --project backend python -m unittest discover -s backend/tests -t backend -p test_merge_acceptance.py
pnpm --dir frontend test:unit
```

The backend exports the actual owner API response from the Codex protected-rename
scenario. The frontend consumes it without adapting the API shape. The new renderer
test currently fails because the API supplies `merge_enrichment` while the shared
component reads `merge`; slice d deliberately leaves this product defect unfixed.
Without the export environment variable, that cross-process test is skipped, just
like the existing `HITL_UI_FIXTURE_PATH` integration. Set both variables during full
acceptance runs to exercise both contracts. This is server-renderer evidence, not
browser or native-runtime proof.
