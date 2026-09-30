# MadiCloud HTTP API

Base path: `/v1`. All request and response bodies are JSON. The API has **no authentication yet**; the server binds to loopback by default for that reason.

## Conventions

### Responses

Every response sets:

| Header | Value |
| --- | --- |
| `Content-Type` | `application/json` (except `204 No Content`) |
| `Cache-Control` | `no-store` |
| `X-Content-Type-Options` | `nosniff` |
| `X-Request-Id` | Request identifier, also written to server logs |

`X-Request-Id` echoes the client's value if it is 1 to 64 characters of `A-Z a-z 0-9 . _ -`; otherwise the server generates one. Quote it when reporting a problem.

Timestamps are RFC 3339 in UTC with up to microsecond precision.

`HEAD` is accepted wherever `GET` is and returns the same status and headers, including `Content-Length`, with no body. Use `curl -I`; `curl -X HEAD` waits for a body and times out.

### Requests

- `POST` bodies must have `Content-Type: application/json`. A `charset` parameter is accepted only if it is `utf-8`.
- Bodies are limited to 64 KiB.
- A body must be exactly one JSON object. Unknown fields, trailing data, and a second object are rejected.

### Errors

Every error has the same shape:

```json
{"error": "invalid_request", "message": "name must be between 3 and 63 characters"}
```

`error` is a stable machine-readable code. `message` is for humans and may change; do not parse it. Messages never contain SQL, driver errors, hostnames, credentials, or stack traces, and do not echo request input.

| HTTP | `error` | Meaning |
| --- | --- | --- |
| 400 | `invalid_request` | Malformed JSON, missing or invalid field, malformed ID, bad query parameter |
| 404 | `not_found` | No such resource, or no such route |
| 405 | `method_not_allowed` | Method not supported on this path; see `Allow` |
| 409 | `conflict` | Name already in use, or the resource has dependents |
| 413 | `request_too_large` | Body over 64 KiB |
| 415 | `unsupported_media_type` | `Content-Type` is not `application/json` |
| 422 | `idempotency_key_reused` | The `Idempotency-Key` was already used with a different request |
| 500 | `internal_error` | Unexpected server error; details are in the server log under the request ID |
| 503 | `unavailable` | Control-plane storage (PostgreSQL) is unreachable; retry later |

## Health and readiness

### `GET /v1/health`

Process liveness. Never touches PostgreSQL.

```json
{"status":"ok","service":"madicloudd","version":"0.1.0","scope":"process"}
```

### `GET /v1/ready`

`200 {"status":"ready"}` when PostgreSQL is reachable and every migration in the binary is applied with matching checksums. Otherwise `503 {"status":"not_ready"}`. The reason is logged, never returned.

## Applications

An application is a named desired-state record. Creating one does not run anything: there is no runtime yet. See [architecture.md](architecture.md).

### Resource

```json
{
  "id": "5c71310c-ad41-4599-99d5-a3baa0374e27",
  "name": "billing-api",
  "desired_state": "active",
  "created_at": "2026-09-30T21:42:06.118001Z",
  "updated_at": "2026-09-30T21:42:06.118001Z"
}
```

| Field | Type | Notes |
| --- | --- | --- |
| `id` | UUID string | Server-generated (PostgreSQL `gen_random_uuid()`, version 4), lowercase |
| `name` | string | See name rules |
| `desired_state` | string | `active` or `deleted`. Only `active` is returned in this release |
| `created_at` | timestamp | |
| `updated_at` | timestamp | Equal to `created_at`; applications cannot be updated yet |

There is no observed state field. Nothing observes applications yet.

### Name rules

- 3 to 63 characters
- lowercase ASCII letters `a-z`, digits `0-9`, and hyphen `-`
- starts with a letter, ends with a letter or digit
- no consecutive hyphens (`--`)
- unique across the cluster
- case-sensitive in the trivial sense: uppercase is rejected, not folded, so every name has one spelling

The 63-character limit is one DNS label. PostgreSQL enforces the same rules with `CHECK` and `UNIQUE` constraints. A name becomes available again once its application is deleted.

### `POST /v1/apps`

Create an application.

```sh
curl -sS -H 'Content-Type: application/json' \
     -H 'Idempotency-Key: 7d4c1a2e-deploy-42' \
     -d '{"name":"billing-api"}' \
     http://127.0.0.1:8080/v1/apps
```

Request body:

```json
{"name": "billing-api"}
```

`201 Created` with the application and `Location: /v1/apps/{id}`.

| Status | `error` | When |
| --- | --- | --- |
| 400 | `invalid_request` | Invalid JSON, unknown field, invalid name, invalid `Idempotency-Key` |
| 409 | `conflict` | Name already in use |
| 413 | `request_too_large` | |
| 415 | `unsupported_media_type` | |
| 422 | `idempotency_key_reused` | |
| 503 | `unavailable` | |

#### Idempotency

`POST` without a key is **not** safe to retry: if the response is lost after the server committed, a retry gets `409 conflict` for the name it just created. Names are deliberately not idempotency keys, because "create this name" and "retry my earlier create" are different intents.

To make a create retryable, send `Idempotency-Key`:

- 1 to 255 visible ASCII characters (`0x21`–`0x7E`), sent once. A random UUID per logical operation is a good choice.
- **Same key, same request** (same `name`, regardless of JSON formatting): returns the original `201` body byte-for-byte, with `Idempotent-Replayed: true`. No second application is created. This holds even if the application was deleted after the first request.
- **Same key, different request**: `422 idempotency_key_reused`.
- **Concurrent requests with one key**: they execute one at a time. The first creates; the rest replay its result.
- **Failures are not recorded.** A request that fails (invalid name, name conflict, database unavailable) leaves no record, so retrying with the same key runs the create again.
- **Retention**: a key replays for 24 hours after the successful request. After that it is forgotten and may be reused for anything. Expired records are removed during later keyed creates.
- **Scope**: keys are scoped to the operation (`applications.create`). There are no user accounts yet; once authentication exists, keys will also be scoped to the caller.

The `madicloud` CLI always sends a fresh key per invocation and retries network failures and 5xx responses up to three times with that key.

### `GET /v1/apps`

List applications, ordered by `name` in byte order (`-` sorts before digits, digits before letters).

```sh
curl -sS 'http://127.0.0.1:8080/v1/apps?limit=2'
```

```json
{
  "applications": [ {"id": "...", "name": "api-app", ...}, {"id": "...", "name": "billing-api", ...} ],
  "next_cursor": "YmlsbGluZy1hcGk"
}
```

| Parameter | Default | Notes |
| --- | --- | --- |
| `limit` | 100 | 1 to 500 |
| `cursor` | none | `next_cursor` from the previous page |

`next_cursor` is present only when more applications followed at the time of the request. If they were deleted before the next request, that page is empty and has no `next_cursor`. Cursors are opaque; do not construct them. Pagination is keyset-based, so applications created or deleted between pages do not cause duplicates or skips among the rest. Unknown or repeated parameters return `400`.

`applications` is always an array, `[]` when empty.

### `GET /v1/apps/{id}`

Return one application. `{id}` must be a canonical UUID (`8-4-4-4-12` hex, either case); anything else is `400 invalid_request` without a database query. An unknown ID is `404 not_found`.

### `DELETE /v1/apps/{id}`

Delete an application. `204 No Content` on success.

| Status | `error` | When |
| --- | --- | --- |
| 400 | `invalid_request` | Malformed ID |
| 404 | `not_found` | No application with this ID, including one already deleted |
| 409 | `conflict` | Other resources depend on it (none exist in this release) |
| 503 | `unavailable` | |

Deletion is idempotent in effect: repeating it never changes state further. The repeat returns `404`, so a client retrying after a lost response should treat `404` as "already gone".

In this release deletion removes the row immediately, because nothing can depend on an application yet. Future dependent resources will reference applications with foreign keys, so a delete with dependents fails atomically with `409` instead of racing with the dependency check. When controllers exist, `DELETE` will record `desired_state: "deleted"` and return before teardown finishes.
