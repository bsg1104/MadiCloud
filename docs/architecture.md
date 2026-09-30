# MadiCloud architecture

This describes what exists now and where it is heading. Anything not listed under "exists" does not exist.

## Exists

```text
                    madicloud CLI
                         |
                         |  HTTP (/v1)
                         v
                  HTTP API server            internal/api
                         |
                  Application service        internal/application
                         |
                  Application repository     internal/database (ApplicationStore)
                         |
                     PostgreSQL
                         |
                   desired state
```

Nothing exists below desired state. There is no scheduler, node agent, container runtime, ingress, or controller.

### Layers

| Layer | Package | Knows about | Does not know about |
| --- | --- | --- | --- |
| CLI | `cmd/madicloud` | argument parsing, output formatting | HTTP details, PostgreSQL |
| API client | `internal/client` | HTTP, JSON, retries with idempotency keys | CLI, PostgreSQL |
| HTTP API | `internal/api` | routing, request decoding, error mapping, request IDs | SQL, the driver |
| Service | `internal/application` | validation, desired-state rules, the `Store` contract | HTTP, PostgreSQL, Docker, scheduling |
| Repository | `internal/database` | SQL, transactions, constraint names, driver errors | HTTP |
| Storage | PostgreSQL | invariants as constraints | |

`internal/database` is the only package that imports the PostgreSQL driver. The service depends on the `application.Store` interface, which exists because it is a real boundary: the PostgreSQL implementation is tested against PostgreSQL, and code above it is tested against an in-memory store with the same semantics (`internal/application/applicationtest`). The in-memory store is never used by `madicloudd`.

The CLI never connects to PostgreSQL. Future MCP access will go through the same HTTP API.

### Invariants and where they are enforced

| Invariant | Go | PostgreSQL |
| --- | --- | --- |
| Name format and length | `application.ValidateName` | `applications_name_format` CHECK |
| Name unique | none; a pre-check would race | `applications_name_key` UNIQUE |
| Desired state is known | `DesiredState.Valid` | `applications_desired_state_valid` CHECK |
| Required fields present | service validation | `NOT NULL` |
| `updated_at >= created_at` | | CHECK |
| No delete while referenced | | foreign keys from dependents (none yet) |
| One create per idempotency key | | advisory lock + primary key on `(operation, key)` |

Go validation gives clear error messages. PostgreSQL constraints make the invariants hold for any writer, including a future second control-plane replica. A constraint violation that reaches the repository is mapped to the same application error the Go check would have produced.

Names use `COLLATE "C"`, so ordering and pagination are byte-wise and do not depend on the database's locale.

## Desired state and observed state

```text
Application desired state        exists (this phase)
        |
        v
PostgreSQL                       exists
        |
        v
Future controllers               do not exist
        |
        v
Future observed state            does not exist
```

**Desired state** is what an operator or agent has asked for: "an application named `billing-api` should exist and be active." It is written only through the API and stored in PostgreSQL.

**Observed state** will be what the platform has seen on real machines: which containers are running, on which nodes, whether they are healthy. It does not exist because nothing runs applications yet. The API has no observed-state field, and the CLI prints "not tracked (no runtime yet)" rather than implying anything is running.

**Reconciliation** will be controllers that read desired state, compare it with observed state, and act to close the gap. Controllers will read and update desired state only through the same service layer or its storage contracts, never by bypassing constraints.

### Application desired-state machine

```text
(none) --create--> active --delete--> deleted --(dependents gone)--> removed
```

- `active`: the application should exist.
- `deleted`: deletion was requested and dependents are being torn down.
- removed: the row is gone and the name is free.

In this phase an application has no dependents, so delete goes from `active` to removed in one statement and `deleted` is never persisted. The value is already allowed by the database constraint so the later change is a behavior change, not a schema change.

## Idempotent creation

A create with `Idempotency-Key` runs in one transaction:

1. `pg_advisory_xact_lock(class, hash(operation, key))`: concurrent requests with the same key queue here.
2. Remove up to 100 expired idempotency records (`FOR UPDATE SKIP LOCKED`, so concurrent creates never block or deadlock on each other's cleanup).
3. Look up a live record for the key. If it exists: same request hash replays the stored result; a different hash is `idempotency_key_reused`.
4. Otherwise insert the application and the record, then commit.

The record and the application commit together. A failed create rolls back both, so failures are never replayed and the retry runs again. The request hash is computed from the validated name, not raw bytes, so reformatted JSON matches.

## Process lifecycle

`madicloudd serve` starts without PostgreSQL. `/v1/health` is process liveness and never touches the database. `/v1/ready` pings PostgreSQL and checks that the schema matches the binary's migrations. Application requests while PostgreSQL is down return `503 unavailable`. The pool reconnects when PostgreSQL returns, with no restart.

Shutdown: stop accepting connections, drain in-flight requests (up to 10 seconds), close the database pool.

`madicloudd migrate` applies migrations explicitly; `serve` never migrates.

## Not yet designed

Authentication and authorization, nodes, scheduling, runtime, ingress, deployments, and MCP. Each will be added as a phase with its own design notes here.
