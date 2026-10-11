# Projects

The project detail page includes the current merge policy, a versioned selector and the server's
read-only protected-path patterns. Policies are **Auto** and **Approval required**. Auto still
requires server merge checks; protected-path changes always require owner approval.

When policy writes are off (the default), the current policy stays visible and the selector and
save action are disabled with an explicit “Editing disabled” explanation. No deployment setting
is enabled by the UI. With writes enabled, Save sends the current version and chosen value.
A conflict or failed/uncertain save refreshes the policy and asks the owner to review it before
saving again. Loading/read failures disable edits; Refresh reads current state without writing.

This selector changes policy only. It does not merge a pull request or approve a pending call.
See [merge policy](merge-policy.md) for server gates and unproved production prerequisites.

## React status (migration F7)

The React projects list, project page, merge policy section and new workspace from a repository
live in `web/src/features/projects`. The list drops a row that breaks the contract and renders the
rest. The page keeps its last loaded project when a refresh fails. The page's task tree is a
marked slot until F6 exports it, and the Projects slot in the app shell is not mounted yet.
Proven by offline component tests against MSW handlers; not proven against a live server.
