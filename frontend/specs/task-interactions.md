# Task Interactions Test Plan

## Application Overview

Tasks in mainloop go through interactive planning phases where users provide input:

- **waiting_questions**: Worker needs clarifying questions answered before proceeding
- **waiting_plan_review**: Implementation plan ready for user approval
- **ready_to_implement**: Plan approved, waiting for user to start implementation

The TasksPanel component handles all these interactive states with rich UI for answering questions, reviewing plans, and controlling task execution.

## Test Scenarios

### 1. Question Answering Flow

**Seed:** tests/seed.spec.ts

#### 1.1 View Question with Options

**Steps:**

1. Expand a task in "waiting_questions" status (NEEDS INPUT badge)
2. Observe the question display

**Expected:**

- Question text displayed clearly
- Multiple choice options shown as clickable buttons
- Custom text input available as alternative
- Question header indicates question number (e.g., "1 of 3")

#### 1.2 Select Option Answer

**Steps:**

1. View a question with multiple options
2. Click one of the option buttons

**Expected:**

- Selected option highlighted with accent color (term-accent)
- Other options become less prominent
- Question collapses to show answer summary
- Next unanswered question auto-expands
- Progress indicator updates

#### 1.3 Provide Custom Text Answer

**Steps:**

1. View a question in expanded state
2. Ignore the option buttons
3. Type custom answer in text input field
4. Press Enter or click OK

**Expected:**

- Custom text saved as the answer
- Option buttons deselected if any were selected
- Question collapses showing the custom text
- Advances to next unanswered question

#### 1.4 Edit Previous Answer

**Steps:**

1. Answer several questions in a task
2. Click on an already-answered question summary

**Expected:**

- Question expands back to edit mode
- Previously selected answer is highlighted
- Can change to different option or custom text
- Other answered questions remain collapsed

#### 1.5 Submit All Answers

**Steps:**

1. Answer all questions for a task
2. Observe the "Continue" button appears
3. Click "Continue" button

**Expected:**

- All answers submitted to backend in single request
- Loading state shown during submission
- Task status changes to "planning" on success
- Question UI replaced with log viewer
- Error displayed if submission fails

### 2. Plan Review Flow

**Seed:** tests/seed.spec.ts

#### 2.1 View Plan Content

**Steps:**

1. Expand a task in "waiting_plan_review" status
2. Observe the plan display

**Expected:**

- Plan rendered with full markdown formatting
- Code blocks styled appropriately
- Scrollable container for long plans
- "Approve Plan" button visible (green styling)
- "Cancel" button visible
- Revision text input field available

#### 2.2 Approve Plan

**Steps:**

1. Review plan in "waiting_plan_review" status
2. Click "Approve Plan" button

**Expected:**

- Loading state during approval submission
- Task status changes to "ready_to_implement" on success
- Success indicator briefly shown
- "Start Implementation" button appears
- Plan content remains visible for reference

#### 2.3 Request Plan Revision

**Steps:**

1. Review plan in "waiting_plan_review" status
2. Type feedback in revision text input
3. Click "Revise" button

**Expected:**

- Feedback text required (button disabled if empty)
- Loading state during submission
- Task status returns to "planning"
- Plan regeneration begins with new context
- Log viewer shows planning activity

#### 2.4 Cancel Plan

**Steps:**

1. Review plan in "waiting_plan_review" status
2. Click "Cancel" button

**Expected:**

- Confirmation dialog may appear
- Task status changes to "cancelled"
- Task moves to failed/cancelled section
- Can no longer interact with task

### 3. Start Implementation Flow

**Seed:** tests/seed.spec.ts

#### 3.1 View Ready to Implement State

**Steps:**

1. Find task in "ready_to_implement" status
2. Observe the task card

**Expected:**

- Status badge shows "READY" or similar
- "Start Implementation" button prominently displayed
- Approved plan summary may be visible
- Cancel option still available

#### 3.2 Start Implementation

**Steps:**

1. Find task in "ready_to_implement" status
2. Click "Start Implementation" button

**Expected:**

- Loading state during transition
- Task status changes to "implementing"
- Log viewer becomes active with live logs
- Implementation progress visible
- Cancel button remains available

### 4. Task Cancellation

**Seed:** tests/seed.spec.ts

#### 4.1 Cancel Active Task

**Steps:**

1. Find an active task (any non-terminal status)
2. Click the cancel button (X icon in header)

**Expected:**

- Confirmation may be requested
- Task status changes to "cancelled"
- Any running operations stop
- Task moves to cancelled/failed section
- Clear indication task was cancelled (not failed)

### 5. Error Handling

**Seed:** tests/seed.spec.ts

#### 5.1 Handle Submission Error

**Steps:**

1. Attempt to submit answers when backend is unreachable
2. Observe error handling

**Expected:**

- Error message displayed to user
- Form state preserved (answers not lost)
- Retry possible without re-entering data
- Clear indication of what went wrong

#### 5.2 Handle Network Timeout

**Steps:**

1. Submit action during slow network conditions
2. Wait for timeout

**Expected:**

- Loading state eventually times out
- Error message about network issue
- Ability to retry the action
- No duplicate submissions

### 6. Real-time Updates

**Seed:** tests/seed.spec.ts

#### 6.1 Status Change via SSE

**Steps:**

1. Have a task in active state
2. Backend updates task status (simulated)
3. Observe inbox updates

**Expected:**

- Task status updates without page refresh
- UI reflects new state automatically
- No jarring transitions or flashing
- Badge counts update appropriately

#### 6.2 New Question Arrives

**Steps:**

1. Task is in "implementing" status
2. Worker asks a new question (backend event)
3. Observe inbox

**Expected:**

- Task status changes to "waiting_questions"
- Task auto-expands to show question
- Notification or highlight draws attention
- Badge count increments
