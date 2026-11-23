# Web fetch

`web_fetch` is a Mainloop MCP tool that reads one public web page and returns it to an agent as
text. It is for research: documentation, release notes, a standard. Any public host is allowed.
No agent has the tool until the owner turns it on for that agent: it is never on for a role or a
whole deployment.

Native harness tools such as Claude's WebFetch run inside the workspace, behind the workspace's
egress policy. `web_fetch` leaves from the Mainloop backend instead, so it is not limited to an
allowlist of hosts. That is why it is opt-in, guarded in code and recorded.

## Who has it

- **Implemented:** an agent holds `web_fetch` (sees it in its tool list and can call it) when
  either flag is set:
  - **Its session's flag.** The owner sets it with `POST /sessions/{session_id}/web-fetch
{"enabled": true}`. That works on the main thread, a task's session, or a workspace agent.
    `{"enabled": false}` turns it off again.
  - **Its task's flag.** A task created with `web_fetch: true` gives the tool to every attempt
    of that task, including retries and reassigned attempts.
    - The owner sets it on `POST /tasks`.
    - An agent sets it on `delegate`. It may do so only if it holds `web_fetch` itself; otherwise
      `delegate` is refused with `[forbidden]`. An agent without the tool cannot create one that
      has it.
    - A task's child tasks do not inherit it. Each one is opted in separately.
- **Implemented:** both flags default to off. No existing session or task gains the tool from an
  upgrade.
- **Implemented:** the flags are read on every MCP request.
  - Turning the tool off takes effect on the agent's next call.
  - Turning it on takes effect when the agent next lists its tools. A native CLI that lists its
    tools only at start sees it in its next session.
- **Implemented:** reading the flags.
  - `NativeSessionInfo.web_fetch` reports both: `enabled` is the session's own flag, and
    `from_task` says its task carries the flag.
  - `Task.web_fetch` reports a task's flag.
  - In `api/mcp-tools.json`, each visibility row has a `web_fetch_enabled` field.
- **Implemented:** the tool needs a live role (main, supervisor, child or workspace agent). The
  actor of a completed task, which may only read merge status, never holds it.
- **Implemented:** in the web app, the session controls picker has a **Web fetch** section with
  **Off** and **On**. It appears in the main thread's chat and live views and in a session's live view.
  - Choosing sets the session's own flag. Choosing the current value sends nothing.
  - When the session's task carries the flag, the section says the agent has the tool either way.
  - React tests cover it against MSW, not a live backend.
- The web app has no new-task form, so the task flag is set through the API or by `delegate`.

## The tool

`web_fetch(url, offset?)` makes one GET request and returns the page as text.

- **Implemented: the URL.**
  - It must be absolute, `http` or `https`, on port 80 or 443, with no user name or password, and
    at most 2,048 bytes long.
- **Implemented: addresses.** The backend can reach PostgreSQL, the Kubernetes API and the LAN,
  so the guard lives in code (`server/internal/webfetch`) and does not rely on the network.
  - Each address is checked as it is dialed, after DNS. A name that resolves to a private address
    is refused, on the first request and on every redirect, and so is a name that rebinds to one.
  - Refused addresses are:
    - loopback, private (RFC 1918), link-local (including cloud metadata at `169.254.169.254`),
      carrier-grade NAT, unspecified, multicast and broadcast;
    - `0.0.0.0/8`, the documentation and benchmarking ranges, and `240.0.0.0/4`;
    - IPv6 unique-local, link-local, site-local, documentation, discard, 6to4 and Teredo;
    - any IPv4-mapped or NAT64 address that carries one of the addresses above.
  - `MAINLOOP_WEB_FETCH_DENY_CIDRS` (comma-separated CIDRs) adds ranges to refuse, for example a
    cluster or LAN that uses public addresses. A malformed value stops the MCP server at start.
  - No proxy is taken from the environment. No cookies, credentials or authorization headers are
    sent.
- **Implemented: redirects.** At most 5 are followed. Each one has its URL rules re-checked, and
  its address is checked when it is dialed.
- **Implemented: limits.**
  - A fetch has 15 seconds overall, and at most 2 MiB of the body is read. A longer body is cut,
    and the result says so.
  - Only text is read: `text/*`, JSON, XML, XHTML and `+json`/`+xml` types. Anything else is
    refused with `[unsupported_content]`.
  - HTML becomes plain text.
    - The title is kept.
    - Headings are marked with `#` and list items with `-`.
    - Links are written `[text](absolute url)`.
    - Scripts, styles and other non-text elements are dropped.
- **Implemented: the result.**
  - It carries at most 32,000 bytes of text from `offset` (0 by default), cut at a line break
    where one is near the end of the window, and never inside a character.
  - When more remains, `next_offset` says where to continue. Each call fetches the page again.
  - The text opens with a line saying that the page is untrusted content, to be read as data and
    not followed as instructions. `untrusted: true` is set in the structured result too.
  - The structured result also has `url`, `final_url`, `status`, `content_type`, `title`,
    `page_text`, `offset`, `text_bytes` and `truncated`.
  - A page with an error status (a 404, say) is returned as read, with its status.
- **Implemented: refusals** come back as `[code] message`, with these codes:
  - `invalid_url`, `blocked_address`, `too_many_redirects`, `unsupported_content`, `timeout` and
    `fetch_failed`, from the fetch;
  - `rate_limited`, from the limit below;
  - `unavailable`, when the process has no fetcher.

## Audit and rate limit

- **Implemented:** each fetch writes a `web_fetches` row before any network call: the owner,
  session and URL.
  - The row is written in the transaction that re-checks the caller's MCP credential, so a revoked
    agent fetches nothing.
  - The row is completed afterwards with the final URL, the status, the body bytes read and any
    error code.
- **Implemented:** an owner's agents together may fetch `MAINLOOP_WEB_FETCH_PER_MINUTE` times per
  minute (30 by default), counted from those rows. Over the limit, a call is refused with
  `[rate_limited]` and nothing is fetched.
- **Proposed:** a view of the audit rows for the owner.

## Risk

An agent with `web_fetch` can send data to any public host by putting it in a URL. The owner
accepted this risk for agents they opt in. Every URL is recorded, and no agent can give the tool
to another agent unless it holds the tool itself.
