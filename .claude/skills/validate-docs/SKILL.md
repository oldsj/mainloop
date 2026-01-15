---
name: validate-docs
description: Validate README features are documented in specs and covered by e2e tests. Use when checking documentation coverage or before merging docs changes.
---

# Validate Documentation

Validate that ALL features claimed in README are documented in specs and tested.

## Philosophy

The README is the product's promise to users. Every feature advertised must be:

1. Specified in `docs/specs/` (source of truth for behavior)
2. Tested in `frontend/tests/` (proof it works)

## Workflow

### Step 1: Extract ALL features from README

Read README.md and identify every feature claim. Look in:

- **How It Works** section - core functionality
- **UI** section - user-facing features
- **Agent Workflow** section - automation features
- Any diagrams, bullet points, or descriptions that promise functionality

For each feature, note:

- What it claims to do
- Whether it has a linked spec (some won't)

### Step 2: Map features to specs

Check if each README feature has a corresponding spec:

| README Feature                             | Expected Spec                                    |
| ------------------------------------------ | ------------------------------------------------ |
| Main thread conversation                   | `docs/specs/chat.md`                             |
| Sessions/background work                   | `docs/specs/sessions.md`                         |
| Mobile/desktop layout                      | `docs/specs/layout.md`                           |
| Agent workflow (spawn → work → PR → close) | `docs/specs/agent-workflow.md`                   |
| Notifications                              | `docs/specs/sessions.md` (notifications section) |

**Flag any README feature without a spec as ERROR.**

### Step 3: Verify specs have testable assertions

For each spec file:

- Check it contains specific, testable statements
- Assertions should mention exact UI text, behaviors, or states
- Vague specs like "works well" are not testable

### Step 4: Map spec assertions to tests

For each bullet point/assertion in a spec, search for a test that verifies it:

```bash
# Example: search for test covering "No sessions yet" message
grep -r "No sessions yet" frontend/tests/
```

Track coverage for each spec assertion.

### Step 5: Output report

```markdown
## Documentation Validation Report

### README Features → Specs

- [x] Main thread conversation → docs/specs/chat.md
- [x] Sessions → docs/specs/sessions.md
- [x] Layout → docs/specs/layout.md
- [ ] **Agent Workflow → NO SPEC** ← ERROR

### Spec Assertions → Tests

#### docs/specs/chat.md (7/9 = 78%)

- [x] "Input field with placeholder" → e2e/user-journey.spec.ts
- [ ] "Messages persist across reloads" → NO TEST

#### docs/specs/sessions.md (23/23 = 100%)

- [x] "No sessions yet" → sessions/01-session-list-empty.spec.ts
      ...

### Summary

| Category                   | Coverage    |
| -------------------------- | ----------- |
| README features with specs | 3/4 (75%)   |
| Spec assertions with tests | 38/40 (95%) |

### Errors

1. README "Agent Workflow" section has no spec
2. chat.md "Messages persist across reloads" has no test
```

### Step 6: Exit status

- **ERROR** if any README feature lacks a spec
- **ERROR** if any spec assertion lacks test coverage
- **SUCCESS** only if fully covered

## Common Gaps to Watch For

1. **Advertised but unspecified** - README promises feature, no spec exists
2. **Specified but untested** - Spec describes behavior, no test verifies it
3. **Workflow features** - Multi-step flows (like agent lifecycle) often lack e2e coverage

## Tests to Flag for Removal

Not all tests are good tests. Flag these as problems:

1. **Tests without specs** - If a test exists but no spec describes the behavior, either:
   - The spec is missing (add it), or
   - The test is testing implementation details (remove it)

2. **Too technical / low-level** - Tests should verify user-facing behavior, not internals:
   - ❌ "store updates when API returns data"
   - ❌ "component re-renders on state change"
   - ✅ "user sees updated session status after refresh"

3. **Testing implementation, not behavior** - Tests coupled to code structure:
   - ❌ "calls fetchSessions with correct params"
   - ❌ "dispatches ACTION_TYPE to store"
   - ✅ "sessions list shows new session after spawning"

4. **Duplicate coverage** - Multiple tests verifying the same user behavior

5. **Tests for removed features** - Spec was removed but test remains

The test suite should read like a user manual, not a code audit.
