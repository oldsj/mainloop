# Inbox Management Test Plan

## Application Overview

Mainloop is an attention management system with a unified inbox. The inbox panel (TasksPanel component) displays:

- **Queue items needing response** - Questions from workers, plan approvals
- **Active tasks** - In-progress work with live logs (status: planning, implementing)
- **Failed jobs** - With retry capability and error details
- **Recently completed** - Items from last 30 minutes
- **History section** - Collapsible, older completed/failed tasks

Layout modes:
- **Desktop (>=768px)**: Chat + always-visible inbox sidebar on the right
- **Mobile (<768px)**: Bottom tab bar switching between [CHAT] and [INBOX] views

## Test Scenarios

### 1. Inbox Panel Visibility

**Seed:** tests/seed.spec.ts

#### 1.1 Desktop Inbox Always Visible

**Steps:**
1. Load application at desktop viewport (1280x720)
2. Wait for app to fully load (header shows "$ mainloop")

**Expected:**
- Inbox panel visible on right side of screen
- Header shows "[INBOX]" with terminal styling
- Project filter dropdown visible in inbox header
- Badge shows attention count if there are items needing action

#### 1.2 Mobile Inbox Tab Navigation

**Steps:**
1. Load application at mobile viewport (375x667)
2. Verify bottom tab bar is visible
3. Tap the [INBOX] tab

**Expected:**
- Bottom tab bar shows [CHAT] and [INBOX] tabs
- [CHAT] tab is active by default
- Tapping [INBOX] shows full-screen inbox panel
- Chat view is hidden when inbox is active
- Badge on inbox tab shows count if items exist

### 2. Queue Item Display

**Seed:** tests/seed.spec.ts

#### 2.1 View Question Queue Item

**Steps:**
1. When a question item exists in inbox (type: question)
2. Observe the queue item card

**Expected:**
- Question text is displayed prominently
- Text input field for response is visible
- SEND button is present
- Timestamp shows when question was asked

#### 2.2 View Plan Review Queue Item

**Steps:**
1. When a plan_review item exists in inbox
2. Click "View plan" button to expand

**Expected:**
- Plan content expands in scrollable container
- Markdown formatting is rendered correctly
- "Approve Plan" button visible (green accent)
- "Cancel" button visible
- Text input for revision feedback available

#### 2.3 Respond to Question

**Steps:**
1. Find a question item in inbox
2. Type response text in the input field
3. Click SEND button (or press Enter)

**Expected:**
- Response is submitted to backend
- Item shows loading state during submission
- On success, item may be removed or show "responded" status
- Input field clears after successful submission

### 3. Task Status Display

**Seed:** tests/seed.spec.ts

#### 3.1 Active Task Indicators

**Steps:**
1. When an active task exists (status: planning or implementing)
2. Observe the task card in inbox

**Expected:**
- Status badge shows with pulse animation (blinking dot)
- Status text shows "PLANNING" or "IMPLEMENTING"
- Task card is expandable (click to show details)
- Cancel button (X icon) visible in task header

#### 3.2 Expand Task to View Logs

**Steps:**
1. Find an active or completed task
2. Click on the task card to expand it

**Expected:**
- Task details section expands
- Log viewer component shows if logs exist
- Logs display with terminal-style formatting
- Scroll position maintained when new logs arrive

#### 3.3 Failed Task Display

**Steps:**
1. When a failed task exists in inbox
2. Observe the task card

**Expected:**
- Status badge shows "FAILED" in error color
- Error message or reason displayed
- Retry button (circular arrow) visible
- Task remains in recent section (doesn't auto-collapse to history)

#### 3.4 Retry Failed Task

**Steps:**
1. Find a failed task in inbox
2. Click the retry button (circular arrow icon)

**Expected:**
- Task status changes from "failed" to "pending"
- Error message is cleared
- Task re-enters the processing queue
- Loading indicator may show during retry initiation

### 4. Recently Completed Section

**Seed:** tests/seed.spec.ts

#### 4.1 View Recent Completions

**Steps:**
1. When tasks have completed within the last 30 minutes
2. Observe the inbox panel

**Expected:**
- Recently completed tasks visible in main inbox area
- Completion timestamp shown for each
- Tasks show "COMPLETED" status badge
- PR link visible if task created a pull request

### 5. History Section

**Seed:** tests/seed.spec.ts

#### 5.1 Expand History

**Steps:**
1. When older completed/failed tasks exist (>30 min old)
2. Find "History (N)" section header where N is count
3. Click the history section header

**Expected:**
- History section expands to show older tasks
- Chevron icon rotates to indicate expanded state
- Older tasks shown with reduced opacity styling
- Section can scroll if many history items

#### 5.2 Collapse History

**Steps:**
1. When history section is expanded
2. Click the "History" section header again

**Expected:**
- History section collapses
- Only recent items remain visible
- Chevron rotates back to collapsed indicator
- Scroll position resets appropriately

### 6. Project Filtering

**Seed:** tests/seed.spec.ts

#### 6.1 Filter Tasks by Project

**Steps:**
1. When multiple projects have tasks
2. Click the project filter dropdown in inbox header
3. Select a specific project

**Expected:**
- Dropdown shows list of projects with task counts
- Selecting a project filters the inbox view
- Only tasks for selected project are shown
- "All Projects" option available to clear filter

### 7. Empty States

**Seed:** tests/seed.spec.ts

#### 7.1 Empty Inbox

**Steps:**
1. When no tasks or queue items exist
2. Observe the inbox panel

**Expected:**
- Empty state message displayed
- No confusing blank space
- Clear indication that inbox is empty
- Possibly suggestion to start a task
