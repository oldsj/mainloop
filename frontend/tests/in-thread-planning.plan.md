# In-Thread Planning Flow Test Plan

## Application Overview

This test plan covers the in-thread planning workflow in mainloop, where planning happens synchronously in the main chat conversation rather than asynchronously via K8s Jobs. The workflow includes:

1. User requests work that requires code changes
2. Claude uses the `start_planning` tool to begin a planning session
3. Claude has read-only access to a cached repo to explore the codebase
4. User and Claude refine the plan interactively in chat
5. User approves the plan, triggering:
   - GitHub issue creation
   - WorkerTask creation (skipping plan phase)
   - Worker K8s Job for implementation only
6. Alternatively, user can cancel planning (no issue created)

The UI implements planning as regular chat messages - no special UI required. Planning status is tracked via PlanningSession in the database.

Key differences from old async planning:
- Planning dialogue is inline in chat (not in worker logs)
- GitHub issue created only on approval (not before planning)
- Worker task starts at READY_TO_IMPLEMENT (skips planning phase)
- Uses cached repos on PVC for fast read access

## Architecture

```
User Chat -> start_planning tool -> PlanningSession created -> Repo cached on PVC
          -> Claude explores codebase (Read, Glob, Grep) -> Plan refined
          -> approve_plan tool -> GitHub issue created -> WorkerTask (skip_plan=True)
          -> K8s Job (implementation only)
```

## Test Scenarios

### 1. Start Planning Flow

**Seed:** Custom seeding via API (no pre-existing planning sessions)

#### 1.1. Start planning with valid repo URL

**File:** `frontend/tests/in-thread-planning/01-start-planning.spec.ts`

**Steps:**
1. Navigate to the app
2. Send a chat message requesting code work: "Add user authentication to https://github.com/test/repo"
3. Wait for Claude to respond
4. Verify Claude uses start_planning tool (visible in response or tool indicator)
5. Verify response mentions planning session started
6. Verify planning session created in database

**Expected Results:**
- Claude recognizes this is a code task
- start_planning tool is invoked
- Response indicates planning has begun
- User sees confirmation message
- PlanningSession record exists with status="active"

#### 1.2. Start planning - ask for repo URL if not provided

**File:** `frontend/tests/in-thread-planning/02-ask-for-repo.spec.ts`

**Steps:**
1. Navigate to the app
2. Send a chat message: "Add user authentication"
3. Wait for Claude to respond
4. Verify Claude asks for the GitHub repository URL
5. Send follow-up with repo URL
6. Verify planning session starts

**Expected Results:**
- Claude doesn't start planning without repo URL
- Claude asks for repository information
- After receiving URL, planning starts

#### 1.3. Start planning - suggest recent repos

**File:** `frontend/tests/in-thread-planning/03-suggest-recent-repos.spec.ts`

**Steps:**
1. Seed recent_repos for the main thread with 2-3 repos
2. Navigate to the app
3. Send a chat message: "Add logging to the backend"
4. Verify Claude mentions the recent repos in response
5. User confirms which repo
6. Verify planning starts with that repo

**Expected Results:**
- Claude sees recent repos in system prompt
- Claude suggests using a recent repo
- User can confirm without typing full URL

### 2. Planning Dialogue

**Seed:** Active planning session with cached repo

#### 2.1. Claude explores codebase during planning

**File:** `frontend/tests/in-thread-planning/04-explore-codebase.spec.ts`

**Steps:**
1. Start a planning session via chat
2. Claude begins exploring the codebase
3. Verify response includes file structure observations
4. Verify Claude references actual code patterns from the repo
5. Send follow-up: "What's the current auth implementation?"
6. Verify Claude can answer using codebase access

**Expected Results:**
- Claude uses Read, Glob, Grep tools on cached repo
- Claude provides accurate codebase analysis
- Follow-up questions work with repo context
- Planning session maintains repo access

#### 2.2. Refine plan with user feedback

**File:** `frontend/tests/in-thread-planning/05-refine-plan.spec.ts`

**Steps:**
1. Start planning session
2. Wait for Claude to present initial plan
3. Send feedback: "I'd prefer to use JWT instead of sessions"
4. Verify Claude updates the plan
5. Verify plan reflects user feedback

**Expected Results:**
- Claude accepts refinement requests
- Plan is updated based on feedback
- User can iterate on plan details
- Conversation stays in planning mode

### 3. Approve Plan

**Seed:** Active planning session with plan ready

#### 3.1. Approve plan creates GitHub issue

**File:** `frontend/tests/in-thread-planning/06-approve-creates-issue.spec.ts`

**Steps:**
1. Have an active planning session with a finalized plan
2. Send message: "Looks good, let's do it"
3. Verify Claude uses approve_plan tool
4. Verify GitHub issue is created
5. Verify response includes issue URL
6. Verify WorkerTask is created with skip_plan=True

**Expected Results:**
- approve_plan tool is invoked with plan text
- GitHub API called to create issue
- Issue contains the plan details
- Response includes issue URL link
- WorkerTask record exists with skip_plan=True
- PlanningSession status="approved"

#### 3.2. Approve plan spawns worker task

**File:** `frontend/tests/in-thread-planning/07-approve-spawns-worker.spec.ts`

**Steps:**
1. Approve a plan (continue from 3.1)
2. Verify WorkerTask is created
3. Verify task has status=READY_TO_IMPLEMENT
4. Verify task has skip_plan=True
5. Verify task has issue_url and issue_number set
6. Verify task appears in Tasks panel

**Expected Results:**
- WorkerTask created immediately on approval
- Task skips planning phase (skip_plan=True)
- Task has pre-populated issue details
- Task visible in UI

#### 3.3. Approve plan compresses context (optional)

**File:** `frontend/tests/in-thread-planning/08-compress-context.spec.ts`

**Steps:**
1. Have a long planning conversation (multiple exchanges)
2. Approve the plan
3. Verify planning messages are tracked
4. Verify compression can be triggered

**Expected Results:**
- Planning message IDs are recorded
- Compression service can summarize planning dialogue
- Main conversation doesn't become bloated

### 4. Cancel Planning

**Seed:** Active planning session

#### 4.1. Cancel planning - no issue created

**File:** `frontend/tests/in-thread-planning/09-cancel-no-issue.spec.ts`

**Steps:**
1. Start a planning session
2. Send message: "Actually, let's cancel this"
3. Verify Claude uses cancel_planning tool
4. Verify response confirms cancellation
5. Verify no GitHub issue was created
6. Verify no WorkerTask was created

**Expected Results:**
- cancel_planning tool is invoked
- PlanningSession status="cancelled"
- No GitHub API calls made
- No WorkerTask record created
- User can start fresh planning later

#### 4.2. Cancel planning mid-conversation

**File:** `frontend/tests/in-thread-planning/10-cancel-mid-conversation.spec.ts`

**Steps:**
1. Start planning session
2. Have 2-3 exchanges about the plan
3. Send: "Never mind, I changed my mind"
4. Verify planning is cancelled cleanly
5. Send new request: "Actually, let's do something else"
6. Verify new conversation works normally

**Expected Results:**
- Cancellation works at any point
- Previous planning state is cleared
- New requests work normally
- No zombie planning sessions

### 5. Error Handling

**Seed:** Various error scenarios

#### 5.1. Invalid repo URL handling

**File:** `frontend/tests/in-thread-planning/11-invalid-repo-url.spec.ts`

**Steps:**
1. Send message: "Add auth to not-a-valid-url"
2. Verify start_planning fails gracefully
3. Verify user gets helpful error message
4. Verify user can retry with valid URL

**Expected Results:**
- Invalid URLs are rejected
- Error message explains the issue
- No PlanningSession created for invalid URL
- User can correct and retry

#### 5.2. Repo clone failure handling

**File:** `frontend/tests/in-thread-planning/12-clone-failure.spec.ts`

**Steps:**
1. Send message with private/inaccessible repo URL
2. Verify clone fails
3. Verify PlanningSession marked as cancelled
4. Verify user gets informative error
5. Verify user can try different repo

**Expected Results:**
- Clone failures are caught
- Session is cancelled (not left hanging)
- Error message explains git clone failed
- User can retry with accessible repo

#### 5.3. GitHub issue creation failure

**File:** `frontend/tests/in-thread-planning/13-issue-creation-failure.spec.ts`

**Steps:**
1. Have active planning session
2. Mock GitHub API to fail
3. Attempt to approve plan
4. Verify error is handled gracefully
5. Verify user can retry

**Expected Results:**
- Issue creation failure is caught
- Error message shown to user
- Planning session not incorrectly marked approved
- User can retry approval

### 6. Session State Management

**Seed:** Various session states

#### 6.1. Only one active session per conversation

**File:** `frontend/tests/in-thread-planning/14-single-active-session.spec.ts`

**Steps:**
1. Start planning session for repo A
2. Before approving/canceling, try to start planning for repo B
3. Verify behavior (either reject or auto-cancel previous)

**Expected Results:**
- Only one active planning session per conversation
- Clear behavior for conflicting requests
- No orphaned sessions

#### 6.2. Session persists across page refresh

**File:** `frontend/tests/in-thread-planning/15-session-persistence.spec.ts`

**Steps:**
1. Start planning session
2. Refresh the page
3. Send follow-up message about the plan
4. Verify planning context is maintained

**Expected Results:**
- PlanningSession stored in database
- Claude session ID enables resumption
- User can continue planning after refresh

### 7. Integration with Existing Flows

**Seed:** Mix of planning and regular chat

#### 7.1. Planning tools don't appear for non-code requests

**File:** `frontend/tests/in-thread-planning/16-no-planning-for-chat.spec.ts`

**Steps:**
1. Send general question: "What is the capital of France?"
2. Verify Claude answers normally
3. Verify no planning tools are invoked
4. Verify no PlanningSession created

**Expected Results:**
- Non-code questions answered normally
- Planning tools not triggered
- Regular chat flow maintained

#### 7.2. Approved plan leads to working implementation

**File:** `frontend/tests/in-thread-planning/17-end-to-end.spec.ts`

**Steps:**
1. Start planning session for test repo
2. Get plan from Claude
3. Approve plan
4. Verify GitHub issue created
5. Verify WorkerTask in READY_TO_IMPLEMENT status
6. Start the worker task
7. Verify implementation proceeds (not planning again)

**Expected Results:**
- Full flow works end-to-end
- Worker starts at implementation phase
- Uses the plan from planning session
- Creates PR based on approved plan

## Files Involved

| File | Role |
|------|------|
| `backend/src/mainloop/services/planning.py` | Core planning service |
| `backend/src/mainloop/services/chat_handler.py` | Planning tools (start/approve/cancel) |
| `backend/src/mainloop/services/repo_cache.py` | PVC-based repo caching |
| `backend/src/mainloop/db/postgres.py` | PlanningSession CRUD |
| `models/src/models/workflow.py` | PlanningSession model |
| `backend/src/mainloop/workflows/worker.py` | skip_plan handling |

## Test Environment Notes

- Tests need access to a test GitHub repo for clone operations
- GitHub token required for issue creation tests
- Test database should be reset between test runs
- Repo cache PVC may need to be cleared between runs
