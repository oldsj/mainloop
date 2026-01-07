# Question Answering Flow Test Plan

## Application Overview

This test plan covers the question answering workflow in mainloop, where worker tasks request input from users before proceeding. The workflow includes:

1. Tasks enter "waiting_questions" status when they need clarification
2. Tasks display with "NEEDS INPUT" badge in the inbox
3. Questions are auto-expanded for user attention
4. Users can select from predefined options or provide custom text answers
5. Questions collapse after answering and advance to the next unanswered question
6. Users can edit previously answered questions
7. Once all questions are answered, a "Continue" button appears to submit all answers
8. After submission, the task transitions to "planning" status and continues execution

The UI implements a progressive disclosure pattern where:

- Only one question is expanded at a time (the currently active one)
- Answered questions show in collapsed summary view with checkmark
- Unanswered questions ahead are dimmed
- Focus automatically advances through questions for smooth completion

Key UI elements tested:

- Question display with numbered badges (1 of N)
- Option buttons with accent highlighting on selection
- Custom text input with auto-focus and Enter key submission
- Progress indicators and question counters
- Answer editing workflow
- Submit button enabling/disabling based on completion
- Loading states during submission
- Error handling and retry capability

## Test Scenarios

### 1. Question Viewing and Display

**Seed:** `frontend/tests/fixtures/seed-data.ts`

#### 1.1. Display task with NEEDS INPUT badge

**File:** `frontend/tests/question-answering/01-display-needs-input-badge.spec.ts`

**Steps:**

1. Seed a task in 'waiting_questions' status with 2 questions using seedTaskWaitingQuestions()
2. Navigate to the mainloop app at /
3. Wait for the app shell to load (heading '$ mainloop' visible)
4. Verify task appears in inbox with 'NEEDS INPUT' badge
5. Verify badge has warning styling (border-term-warning text-term-warning)
6. Verify task is auto-expanded (expandedTaskIds includes task ID)

**Expected Results:**

- Task card is visible in inbox
- Badge displays 'NEEDS INPUT' text
- Badge has yellow/warning color styling
- Task is automatically expanded to show questions (user doesn't need to click)
- First question is visible and expanded

#### 1.2. Display first unanswered question expanded

**File:** `frontend/tests/question-answering/02-first-question-expanded.spec.ts`

**Steps:**

1. Seed a task with 3 questions using seedTaskWaitingQuestions() with custom questions array
2. Navigate to the app
3. Locate the expanded task
4. Verify the first question is in expanded state
5. Verify question number badge shows '1' with accent border
6. Verify question header is displayed (e.g., 'Authentication Method')
7. Verify question text is fully visible
8. Verify all option buttons are displayed
9. Verify custom text input is visible with placeholder 'Or type a custom answer...'
10. Verify second and third questions are visible but dimmed (opacity-40)
11. Verify future questions show number badges but are not expanded

**Expected Results:**

- Only the first question is shown in expanded state
- Question displays numbered badge (1) with term-accent border
- Question header is shown in a badge with term-accent styling
- Question text is clearly readable
- All option buttons are rendered and clickable
- Custom input field is present and enabled
- Input field has auto-focus
- Questions 2 and 3 are visible but dimmed (40% opacity)
- Future questions show only number badge and header, not full content

#### 1.3. Display question with multiple choice options

**File:** `frontend/tests/question-answering/03-display-options.spec.ts`

**Steps:**

1. Seed a task with a question that has 3 options: 'Yes', 'No', 'Maybe'
2. Navigate to the app
3. Expand the task if not auto-expanded
4. Locate the active question section
5. Count the option buttons displayed
6. Verify each option shows its label text
7. Verify options have border-term-border styling (unselected state)
8. Verify options have hover effect (hover:border-term-accent)
9. Verify all options are enabled (not disabled)

**Expected Results:**

- Three option buttons are visible
- Each button displays correct label: 'Yes', 'No', 'Maybe'
- Buttons have default border styling (border-term-border)
- Buttons change border on hover to term-accent color
- All buttons are clickable (not disabled)
- Buttons are arranged horizontally with gap spacing

#### 1.4. Display question counter and progress

**File:** `frontend/tests/question-answering/04-question-counter.spec.ts`

**Steps:**

1. Seed a task with exactly 5 questions
2. Navigate to the app
3. Locate the first expanded question
4. Verify the question number badge shows '1'
5. Verify all 5 question placeholders are visible in the list
6. Count total number of question elements (should be 5)
7. Verify questions 2-5 are in dimmed state

**Expected Results:**

- First question shows numbered badge with '1'
- All 5 questions are rendered in the list
- Questions 2-5 have reduced opacity (dimmed)
- User can see total number of questions to answer
- Progress is visually clear (1 active out of 5 total)

### 2. Answering Questions with Options

**Seed:** `frontend/tests/fixtures/seed-data.ts`

#### 2.1. Select option and auto-advance to next question

**File:** `frontend/tests/question-answering/05-select-option-advance.spec.ts`

**Steps:**

1. Seed a task with 3 questions
2. Navigate to the app
3. Verify first question (q1) is expanded
4. Click the first option button (e.g., 'Yes')
5. Wait for UI update
6. Verify q1 collapses to summary view
7. Verify q1 shows checkmark (✓) icon
8. Verify q1 summary displays selected answer
9. Verify q2 automatically expands (becomes active)
10. Verify q3 remains dimmed
11. Verify Continue button is NOT visible yet (not all answered)

**Expected Results:**

- First question transitions from expanded to collapsed state
- Collapsed question shows green checkmark (text-term-accent-alt)
- Selected answer 'Yes' is displayed in the summary
- Question 2 automatically becomes the active question
- Question 2 expands with all its options visible
- Question 3 stays in future/dimmed state
- Custom input in q2 receives auto-focus
- No submit button appears until all questions answered

#### 2.2. Select different options for multiple questions

**File:** `frontend/tests/question-answering/06-select-multiple-options.spec.ts`

**Steps:**

1. Seed a task with 2 questions, each with different options
2. Navigate to the app
3. On question 1, click 'JWT' option
4. Wait for q2 to expand
5. On question 2, click 'Yes' option
6. Verify both questions show in collapsed summary state
7. Verify q1 summary shows 'JWT'
8. Verify q2 summary shows 'Yes'
9. Verify both have checkmark icons
10. Verify Continue button appears

**Expected Results:**

- Question 1 collapses with 'JWT' displayed
- Question 2 collapses with 'Yes' displayed
- Both questions show green checkmarks
- Both answers are preserved in the UI
- Continue button becomes visible
- Continue button has term-accent-alt styling
- Continue button is enabled

#### 2.3. Option selection highlights correctly

**File:** `frontend/tests/question-answering/07-option-highlight.spec.ts`

**Steps:**

1. Seed a task with one question having 3 options
2. Navigate to the app
3. Locate the active question with options
4. Click the second option button
5. Verify clicked option has accent border (border-term-accent)
6. Verify clicked option has accent text color (text-term-accent)
7. Verify clicked option has accent background (bg-term-accent/10)
8. Verify other options remain with default border (border-term-border)
9. Verify only one option is highlighted at a time

**Expected Results:**

- Selected option button changes to accent color scheme
- Selected option has border-term-accent class
- Selected option has text-term-accent class
- Selected option has subtle background tint (bg-term-accent/10)
- Unselected options keep default styling
- Selection state is visually distinct and clear
- Only the clicked option shows selection styling

### 3. Custom Text Answers

**Seed:** `frontend/tests/fixtures/seed-data.ts`

#### 3.1. Type custom answer and submit with Enter

**File:** `frontend/tests/question-answering/08-custom-answer-enter.spec.ts`

**Steps:**

1. Seed a task with 2 questions
2. Navigate to the app
3. Locate the custom text input field in the first question
4. Verify input has auto-focus
5. Type 'My custom authentication approach' into the input
6. Verify 'OK' button appears next to the input
7. Press Enter key
8. Verify question 1 collapses
9. Verify custom text 'My custom authentication approach' appears in summary
10. Verify question 2 auto-expands

**Expected Results:**

- Input field automatically receives focus on mount
- Typed text appears in the input field
- OK button becomes visible when text is present
- Pressing Enter advances to next question
- Question collapses to summary view
- Custom text is displayed in the collapsed summary
- Second question becomes active
- Custom answer is preserved (not lost)

#### 3.2. Type custom answer and click OK button

**File:** `frontend/tests/question-answering/09-custom-answer-ok-button.spec.ts`

**Steps:**

1. Seed a task with 1 question
2. Navigate to the app
3. Focus the custom text input
4. Type 'OAuth 2.0 with PKCE'
5. Verify OK button is visible and enabled
6. Click the OK button
7. Verify question collapses
8. Verify 'OAuth 2.0 with PKCE' is shown in summary
9. Verify checkmark appears
10. Verify Continue button appears (all questions answered)

**Expected Results:**

- OK button appears when input has text
- OK button has term-accent border and text
- Clicking OK collapses the question
- Custom answer is displayed in summary
- Checkmark icon is shown
- Continue button becomes visible
- Continue button is enabled for submission

#### 3.3. Custom answer clears selected option

**File:** `frontend/tests/question-answering/10-custom-clears-option.spec.ts`

**Steps:**

1. Seed a task with 1 question with options
2. Navigate to the app
3. Click an option button (e.g., 'Yes')
4. Verify option is highlighted
5. Immediately type in the custom input field: 'Actually, I prefer a different approach'
6. Verify the previously selected option is no longer highlighted
7. Verify only custom text is considered as the answer
8. Collapse the question by pressing Enter or clicking OK
9. Verify summary shows the custom text, not the option

**Expected Results:**

- Option button initially shows selected state
- When user types in custom field, option deselects
- Option button returns to default styling (border-term-border)
- Custom text takes precedence over option selection
- Only custom answer is shown in the summary
- No option remains in selected state
- Answer state correctly reflects custom input

#### 3.4. Option selection clears custom text

**File:** `frontend/tests/question-answering/11-option-clears-custom.spec.ts`

**Steps:**

1. Seed a task with 1 question
2. Navigate to the app
3. Type 'Custom answer here' in the input field
4. Verify text appears in input
5. Click an option button (e.g., 'No')
6. Verify question collapses and auto-advances
7. Verify summary shows 'No' (the option), not the custom text

**Expected Results:**

- Custom text is initially in the input field
- Clicking an option triggers selection
- Question collapses with the option as the answer
- Custom text is not used/displayed
- Summary shows option label 'No'
- Custom input state is cleared

#### 3.5. Empty custom input does not show OK button

**File:** `frontend/tests/question-answering/12-empty-input-no-ok.spec.ts`

**Steps:**

1. Seed a task with 1 question
2. Navigate to the app
3. Locate the custom text input
4. Verify input is empty
5. Verify OK button is NOT visible
6. Type a single character 'a'
7. Verify OK button appears
8. Clear the input (delete the character)
9. Verify OK button disappears again

**Expected Results:**

- OK button is not rendered when input is empty
- OK button appears as soon as text is entered
- OK button disappears when input is cleared
- Button visibility is reactive to input value
- User cannot submit empty custom answer

### 4. Editing Previous Answers

**Seed:** `frontend/tests/fixtures/seed-data.ts`

#### 4.1. Click answered question to edit

**File:** `frontend/tests/question-answering/13-edit-answered-question.spec.ts`

**Steps:**

1. Seed a task with 3 questions
2. Navigate to the app
3. Answer question 1 by selecting an option
4. Answer question 2 by typing custom text
5. Verify both are collapsed with summaries
6. Verify question 3 is now active
7. Click on the collapsed question 1 summary
8. Verify question 1 expands back to edit mode
9. Verify question 3 remains visible but question 1 is now active
10. Verify previous answer is still highlighted/shown

**Expected Results:**

- Clicking collapsed question summary re-expands it
- Question transitions from summary to edit mode
- Previously selected option is still highlighted
- Question becomes the active question (editingQuestionId is set)
- Other collapsed questions remain collapsed
- Currently active question (q3) loses focus
- User can change their answer

#### 4.2. Change answer from option to custom text

**File:** `frontend/tests/question-answering/14-change-option-to-custom.spec.ts`

**Steps:**

1. Seed a task with 2 questions
2. Navigate to the app
3. Select 'Yes' option for question 1
4. Wait for q1 to collapse
5. Click on q1 summary to edit
6. Verify 'Yes' option is highlighted
7. Type 'Actually I want a custom approach' in the custom input
8. Press Enter to confirm
9. Verify q1 summary now shows the custom text
10. Verify 'Yes' is no longer the answer

**Expected Results:**

- Question re-opens with previous option selected
- User can type custom text to override option
- Custom text clears the option selection
- Pressing Enter saves the new custom answer
- Summary updates to show custom text
- Previous option selection is replaced

#### 4.3. Change answer from custom text to option

**File:** `frontend/tests/question-answering/15-change-custom-to-option.spec.ts`

**Steps:**

1. Seed a task with 2 questions
2. Navigate to the app
3. Type 'My custom answer' for question 1 and press Enter
4. Wait for q1 to collapse showing custom text
5. Click on q1 summary to re-edit
6. Verify custom text is in the input field
7. Click the 'No' option button
8. Verify question auto-advances
9. Verify q1 summary now shows 'No'
10. Verify custom text is no longer the answer

**Expected Results:**

- Question re-opens with custom text in input
- Clicking an option overrides custom text
- Question advances to next unanswered
- Summary updates to show option label
- Custom text is cleared from state
- Answer correctly changes to the option

#### 4.4. Edit middle question while others remain answered

**File:** `frontend/tests/question-answering/16-edit-middle-question.spec.ts`

**Steps:**

1. Seed a task with 3 questions
2. Navigate to the app
3. Answer all 3 questions in order
4. Verify all 3 show collapsed with checkmarks
5. Verify Continue button is visible
6. Click on question 2 summary to edit
7. Verify q2 expands to edit mode
8. Verify q1 and q3 remain collapsed with checkmarks
9. Change the answer for q2
10. Press Enter or click to confirm
11. Verify q2 collapses again
12. Verify all 3 questions still answered
13. Verify Continue button remains visible

**Expected Results:**

- Only question 2 expands for editing
- Questions 1 and 3 stay collapsed
- User can edit middle question without affecting others
- After editing, q2 collapses back to summary
- All questions remain in answered state
- Continue button stays enabled
- No loss of data in other questions

### 5. Submitting Answers

**Seed:** `frontend/tests/fixtures/seed-data.ts`

#### 5.1. Continue button appears when all answered

**File:** `frontend/tests/question-answering/17-continue-button-appears.spec.ts`

**Steps:**

1. Seed a task with 2 questions
2. Navigate to the app
3. Verify Continue button is NOT visible initially
4. Answer question 1 (select an option)
5. Verify Continue button is still NOT visible (q2 not answered)
6. Answer question 2 (type custom text and press Enter)
7. Verify Continue button appears
8. Verify button text is 'Continue →'
9. Verify button has term-accent-alt styling
10. Verify button is enabled

**Expected Results:**

- Continue button only appears after ALL questions answered
- Button is not visible when any question is unanswered
- Button appears immediately after last question is answered
- Button has green accent styling (term-accent-alt)
- Button displays 'Continue →' with arrow
- Button is in enabled state
- Cancel button also appears alongside Continue

#### 5.2. Click Continue button to submit answers

**File:** `frontend/tests/question-answering/18-submit-answers.spec.ts`

**Steps:**

1. Seed a task with 2 questions using seedTaskWaitingQuestions()
2. Navigate to the app
3. Answer question 1 with 'JWT' option
4. Answer question 2 with 'Yes' option
5. Verify Continue button is visible and enabled
6. Click Continue button
7. Verify button shows loading state ('Submitting...')
8. Verify button is disabled during submission
9. Wait for API response (task status changes to 'planning')
10. Verify task no longer shows questions UI
11. Verify task status badge changes from 'NEEDS INPUT' to 'PLANNING'
12. Verify log viewer appears for the task

**Expected Results:**

- Continue button changes text to 'Submitting...'
- Button is disabled during API call
- API request is made to /tasks/{taskId}/answer with answers object
- Answers payload includes: {q1: 'JWT', q2: 'Yes'}
- After successful submission, task status updates
- Questions UI is replaced with log viewer
- Task badge updates to show 'PLANNING'
- Local state (selectedAnswers, customQuestionInputs) is cleared
- No errors are shown

#### 5.3. Submit with mix of option and custom answers

**File:** `frontend/tests/question-answering/19-submit-mixed-answers.spec.ts`

**Steps:**

1. Seed a task with 3 questions
2. Navigate to the app
3. Answer q1 with option 'JWT'
4. Answer q2 with custom text 'Rate limit to 100 req/min'
5. Answer q3 with option 'Maybe'
6. Click Continue button
7. Monitor network request payload
8. Verify answers object contains all 3 answers
9. Verify q1 answer is 'JWT' (option)
10. Verify q2 answer is 'Rate limit to 100 req/min' (custom)
11. Verify q3 answer is 'Maybe' (option)
12. Verify task transitions to planning status

**Expected Results:**

- All three answers are included in submission
- Both option selections and custom text are sent
- Answer format is correct: {q1: 'JWT', q2: 'Rate limit to 100 req/min', q3: 'Maybe'}
- API accepts the mixed answer types
- Task status successfully updates
- No data loss between different answer types

#### 5.4. Continue button disabled during submission

**File:** `frontend/tests/question-answering/20-button-disabled-during-submit.spec.ts`

**Steps:**

1. Seed a task with 1 question
2. Navigate to the app
3. Answer the question
4. Click Continue button
5. Immediately verify button is disabled
6. Verify button text shows 'Submitting...'
7. Verify user cannot click button again
8. Wait for submission to complete
9. Verify task transitions away from questions UI

**Expected Results:**

- Button becomes disabled immediately on click
- Button text changes to 'Submitting...' for user feedback
- Multiple clicks are prevented (no duplicate submissions)
- Disabled state persists until API response
- After response, UI transitions to next state
- No double submission occurs

#### 5.5. Cancel button appears with Continue

**File:** `frontend/tests/question-answering/21-cancel-button.spec.ts`

**Steps:**

1. Seed a task with 2 questions
2. Navigate to the app
3. Answer both questions
4. Verify Continue button appears
5. Verify Cancel button appears alongside Continue
6. Verify Cancel button has muted styling (text-term-fg-muted)
7. Verify Cancel button has error hover (hover:text-term-error)
8. Click Cancel button
9. Verify confirmation dialog may appear (browser confirm)
10. If confirmed, verify task is cancelled

**Expected Results:**

- Cancel button is visible when all questions answered
- Cancel button has subtle default styling
- Cancel button shows error color on hover
- Clicking Cancel may show confirmation
- If confirmed, task transitions to cancelled state
- User has option to abort question flow

### 6. Error Handling

**Seed:** `frontend/tests/fixtures/seed-data.ts`

#### 6.1. Handle submission error gracefully

**File:** `frontend/tests/question-answering/22-handle-submission-error.spec.ts`

**Steps:**

1. Seed a task with 1 question
2. Navigate to the app
3. Answer the question
4. Intercept the API call to force it to fail with 500 error
5. Click Continue button
6. Wait for error
7. Verify error is logged to console
8. Verify button returns to enabled state
9. Verify button text returns to 'Continue →'
10. Verify question answers are still preserved
11. Verify user can retry submission

**Expected Results:**

- Error is caught and logged
- Button state resets after error
- Loading state ends
- User answers are not lost
- Questions remain in answered state
- Continue button is clickable again
- User can attempt resubmission
- No data loss occurs

#### 6.2. Handle network timeout during submission

**File:** `frontend/tests/question-answering/23-handle-network-timeout.spec.ts`

**Steps:**

1. Seed a task with 2 questions
2. Navigate to the app
3. Answer both questions
4. Intercept API call to delay response beyond timeout
5. Click Continue button
6. Wait for timeout to occur
7. Verify loading state eventually ends
8. Verify answers are preserved
9. Verify user can retry

**Expected Results:**

- Timeout is handled gracefully
- Loading state does not persist indefinitely
- User gets feedback about the failure
- Answers remain in UI
- Retry is possible
- No UI freeze or hang

#### 6.3. Validation - prevent submission with unanswered questions

**File:** `frontend/tests/question-answering/24-prevent-incomplete-submission.spec.ts`

**Steps:**

1. Seed a task with 3 questions
2. Navigate to the app
3. Answer only questions 1 and 2
4. Verify Continue button is NOT visible
5. Verify question 3 is active and unanswered
6. Attempt to trigger submission programmatically (if possible)
7. Verify submission does not occur
8. Answer question 3
9. Verify Continue button now appears
10. Verify submission is now possible

**Expected Results:**

- Continue button only shows when all questions answered
- Cannot submit with incomplete answers
- UI prevents premature submission
- Last question must be answered
- Button appears only after completion
- Validation is enforced client-side

### 7. Real-time Updates and State Management

**Seed:** `frontend/tests/fixtures/seed-data.ts`

#### 7.1. Task auto-expands on page load when needing input

**File:** `frontend/tests/question-answering/25-auto-expand-on-load.spec.ts`

**Steps:**

1. Seed a task in waiting_questions status
2. Navigate to the app
3. Wait for app to load
4. Verify task is automatically expanded (without user click)
5. Verify first question is visible
6. Verify task is in the expandedTaskIds set
7. Verify task is also in autoExpandedTaskIds set (tracked separately)

**Expected Results:**

- Task needing input auto-expands on page load
- User doesn't need to click to see questions
- First question is immediately visible
- Auto-expansion is tracked separately (autoExpandedTaskIds)
- This allows user to manually collapse if desired
- Task won't re-expand if user manually collapsed it

#### 7.2. User can manually collapse auto-expanded task

**File:** `frontend/tests/question-answering/26-manual-collapse.spec.ts`

**Steps:**

1. Seed a task with questions
2. Navigate to the app
3. Verify task is auto-expanded
4. Click on the task header to collapse it
5. Verify task collapses (questions hidden)
6. Verify task is removed from expandedTaskIds
7. Reload the page
8. Verify task does NOT auto-expand again (respects user preference in session)

**Expected Results:**

- Auto-expanded task can be manually collapsed
- Clicking header toggles expansion
- Collapsed state is maintained
- Task stays collapsed after reload (in same session)
- User preference is respected
- Auto-expansion only happens once per task

#### 7.3. State persists when navigating away and back

**File:** `frontend/tests/question-answering/27-state-persistence.spec.ts`

**Steps:**

1. Seed a task with 3 questions
2. Navigate to the app
3. Answer question 1 and question 2
4. Leave question 3 unanswered
5. Navigate to a different page or refresh
6. Navigate back to the app
7. Verify questions 1 and 2 answers are LOST (client-side state)
8. Verify all questions are back to unanswered state
9. This confirms state is NOT persisted to backend until submission

**Expected Results:**

- Partial answers are NOT saved to backend
- Client-side state is lost on refresh
- User must complete all questions in one session
- This is expected behavior (no auto-save)
- Submission is atomic (all or nothing)

#### 7.4. Multiple tasks with questions can coexist

**File:** `frontend/tests/question-answering/28-multiple-tasks.spec.ts`

**Steps:**

1. Seed two different tasks, both in waiting_questions status
2. Navigate to the app
3. Verify both tasks appear in inbox
4. Verify both show NEEDS INPUT badges
5. Expand first task, answer its questions
6. Click Continue on first task
7. Verify first task transitions away
8. Verify second task remains with questions
9. Answer second task's questions
10. Verify both tasks can be processed independently

**Expected Results:**

- Multiple tasks with questions can exist simultaneously
- Each task maintains its own state
- Answering one task doesn't affect another
- State is isolated per task ID
- Both tasks can be submitted independently
- No cross-contamination of answers

### 8. Accessibility and Keyboard Navigation

**Seed:** `frontend/tests/fixtures/seed-data.ts`

#### 8.1. Custom input receives auto-focus

**File:** `frontend/tests/question-answering/29-input-autofocus.spec.ts`

**Steps:**

1. Seed a task with 1 question
2. Navigate to the app
3. Wait for task to auto-expand
4. Verify the custom text input has focus
5. Type text without clicking the input first
6. Verify text appears (confirming focus worked)

**Expected Results:**

- Input field automatically receives focus
- User can start typing immediately
- No click required to focus input
- Autofocus uses the autofocus action (50ms delay)
- Focus is set after DOM is ready

#### 8.2. Enter key submits custom answer and advances

**File:** `frontend/tests/question-answering/30-enter-key-advances.spec.ts`

**Steps:**

1. Seed a task with 2 questions
2. Navigate to the app
3. Type custom text in question 1 input
4. Press Enter key
5. Verify question 1 collapses
6. Verify question 2 expands and its input receives focus
7. Type text in question 2 input
8. Press Enter key
9. Verify question 2 collapses
10. Verify Continue button appears

**Expected Results:**

- Enter key acts as submit for current question
- Question advances to next on Enter
- Focus moves to next question's input
- Keyboard-only workflow is smooth
- No mouse required to complete questions
- Enter on last question shows Continue button

#### 8.3. Tab key navigation works correctly

**File:** `frontend/tests/question-answering/31-tab-navigation.spec.ts`

**Steps:**

1. Seed a task with 1 question with 3 options
2. Navigate to the app
3. Press Tab key multiple times
4. Verify focus moves through: option buttons, custom input, Cancel button
5. Verify option buttons have tabindex (clickable via Enter when focused)
6. Test that option buttons are NOT focusable via Tab (tabindex={-1})
7. Verify custom input IS focusable via Tab

**Expected Results:**

- Tab navigation follows logical order
- Option buttons have tabindex={-1} (not in tab order)
- Custom input is in tab order
- Focus indicators are visible
- Keyboard navigation is intuitive
- Option buttons excluded from tab order to streamline keyboard flow

#### 8.4. Keyboard event propagation handled correctly

**File:** `frontend/tests/question-answering/32-event-propagation.spec.ts`

**Steps:**

1. Seed a task with 1 question
2. Navigate to the app
3. Focus the custom input field
4. Press various keys (arrow keys, space, etc.)
5. Verify events don't bubble up to parent handlers
6. Verify input field captures keydown events
7. Type text and press Enter
8. Verify Enter is handled by input's onkeydown, not parent

**Expected Results:**

- Input field stops event propagation (e.stopPropagation())
- Parent keydown handlers don't interfere
- Typing works normally without side effects
- Enter key works as expected (advances question)
- No unintended behavior from event bubbling
