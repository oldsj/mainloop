# 003 - Plan First, Code Second

*Why AI workers should plan in issues, not jump straight to code*

---

## The Code-First Trap

There's a tempting pattern when working with AI: describe what you want, hit enter, watch code appear.

It's magical the first few times. Claude generates a complete React component. Cursor autocompletes an entire function. The productivity feels real.

Then you try to merge it. And everything falls apart.

The code works in isolation but doesn't fit your architecture. It uses patterns you've explicitly avoided. It solves the wrong problem elegantly. You spend more time fixing the AI's work than you would have writing it yourself.

This is the code-first trap: **AI can write code faster than humans, but it can't know what code to write**.

## Planning as Thinking Out Loud

The solution is embarrassingly simple: make the AI plan before it codes.

In mainloop, workers follow a strict workflow:

1. **Plan in issue** — Create/update a GitHub issue with problem analysis and proposed approach
2. **Draft PR** — Implement the plan in a draft pull request
3. **Iterate (CI loop)** — Fix failures until checks pass
4. **Ready for review** — Only then does the human get involved

The issue is where the AI "thinks out loud." It's not a formality — it's where most of the value is created.

```
┌─────────────┐     ┌─────────────┐     ┌─────────────┐     ┌─────────────┐
│   Planning  │────►│    Draft    │────►│  Iteration  │────►│   Review    │
│  (GH Issue) │     │    (PR)     │     │  (CI Loop)  │     │   (Human)   │
└─────────────┘     └─────────────┘     └─────────────┘     └─────────────┘
```

## Why Issues, Not Just Comments

GitHub issues have properties that make them perfect for AI planning:

**Visibility**: Issues are first-class objects. You can search them, label them, link them. A plan in an issue is discoverable; a plan buried in a chat log is lost.

**Editability**: The AI can revise the plan as it learns more about the codebase. The issue shows the evolution of understanding.

**Linkage**: PRs reference issues. Commits reference issues. The paper trail is automatic.

**Review before execution**: You can approve or modify the plan *before* any code is written. Course correction is cheap here; it's expensive after 500 lines of implementation.

## Humans at High-Leverage Points

The goal isn't to review every line of code. That defeats the purpose of AI assistance.

The goal is to keep humans at **high-leverage decision points**:

- **Plan approval**: Does this approach make sense for our architecture?
- **Direction changes**: "Actually, let's use a different pattern"
- **Final review**: Does the implementation match the intent?

Everything else — the iteration, the debugging, the CI failures — the AI handles autonomously.

This is how human attention stays protected. You're not reviewing every commit. You're reviewing the plan (cheap to change) and the final result (after the AI has iterated to a working state).

## The Draft PR Pattern

Draft PRs are the AI's workspace. They're explicitly "not ready" and serve several purposes:

**Transparency**: You can watch the AI work if you want. The PR shows commits in progress, comments explaining decisions, CI results.

**Iteration space**: The AI can push broken code, see CI fail, fix it, push again. This is expected and encouraged. Draft PRs signal "work in progress."

**Context preservation**: When the AI writes a commit message explaining why it changed something, that context lives in the PR forever. Future you (or future AI) can understand the evolution.

**Graceful handoff**: When ready, the AI marks the PR ready for review. The human reviews a complete, tested, CI-green implementation — not a work in progress.

## What This Looks Like in Practice

When you tell mainloop "add error handling to the /users endpoint":

1. **Worker spawns** and reads the codebase
2. **Issue created**: "Add error handling to /users endpoint"
   - Analysis of current error handling patterns
   - Proposal for consistent approach
   - List of files to modify
3. **You review** (optional): looks good, proceed
4. **Draft PR created**: Implements the plan
5. **CI runs**: Tests fail (missing test case)
6. **Worker fixes**: Adds test, pushes
7. **CI passes**: Worker marks PR ready
8. **You review**: Final approval

Total human attention: 2 minutes of review on a clear plan and a working implementation. The AI did the hours of grunt work in between.

## The Compound Effect

This pattern compounds over time:

- **Plans create precedent**: "Handle errors like issue #42" becomes possible
- **PRs document decisions**: Why was this approach chosen over alternatives?
- **CI gates quality**: Nothing reaches review without passing checks
- **History builds context**: The Temporal Pyramid (see entry 002) preserves all this

Your codebase develops a memory of how decisions were made, not just what code exists. AI workers can reference past approaches. Humans can trust the process.

## The Meta-Lesson

The deeper insight: **AI is good at execution, humans are good at direction**.

Code-first AI usage conflates these. It treats humans as code reviewers when they should be architects.

Plan-first AI usage separates them. The human provides direction (approve the plan). The AI provides execution (write the code). The result is a collaboration that plays to each party's strengths.

---

*Next entry: [004 - Spawning Workers](004-spawning-workers.md) — how Claude decides when to spawn workers and how the handoff works.*

---

**Building mainloop in public. If structured AI workflows resonate, follow along.**

#buildinpublic #AI #developertools #workflow #planning
