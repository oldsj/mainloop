# API errors

Every response the API sends is declared in `api/openapi.yaml`: its status, its content type and
its body. A test that serves a response the contract does not declare fails (see
`server/internal/app/apicheck`).

## Bodies

- Most errors are `HTTPError`: `{"detail": "<message>"}`.
- A `422` the client can fix field by field is `HTTPValidationError`:
  `{"detail": [{"loc": [...], "msg": "...", "type": "..."}]}`. The request validator and the
  domain handlers (a blank chat message, a bad schedule config) answer with this one shape. A
  domain refusal has an empty `loc`.
- Task and project-provider refusals carry a stable reason: `{"detail": {"reason": "<code>"}}`.

## Statuses that apply to many routes

- `413` on any route with a request body: the body is over the 10 MiB cap.
- `503` when the service behind a route is not wired in this process. The workspace, task,
  schedule and merge-detail routes declare it, as do `GET /events`, `GET /health`, the owner-write
  routes (`POST /hitl/{request_id}/respond`, `PUT /projects/{project_id}/merge-policy`).
- `501` is the fallback for an operation in the contract that has no handler. It is not declared
  because no released route answers it.

## One path

A handler reports an error by returning an `*apierr.Error` (`server/internal/apierr`), never a
per-status typed response; the server writes it. Any other error becomes `500` with a fixed body
and is logged.
