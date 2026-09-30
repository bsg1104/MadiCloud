# MadiCloud

MadiCloud is a self-hosted application cloud I'm building from scratch. The goal is simple to say and hard to do: take a handful of ordinary Linux machines and turn them into a place where you can deploy and run apps, the way you would on Heroku or Fly, except you own the whole thing.

No AWS, no Kubernetes, no Nomad underneath. MadiCloud does its own scheduling, keeps its own state, and decides for itself what should be running where. It's also designed from day one to be driven by AI agents as well as people, which is why the API is strict about validation, retries, and error messages.

## Where things stand

It's early. Right now there's a control plane and a CLI, and you can manage **applications** with them. What you can't do yet is run anything.

That's on purpose. When you create an app, MadiCloud records that you *want* it to exist. Actually starting containers is a later phase. I'd rather the system say "not running" honestly than pretend.

What works today:

- `madicloudd`, the control-plane server, with a small HTTP API
- `madicloud`, a CLI that talks to that API
- PostgreSQL for storing state, with versioned migrations
- Health and readiness checks that behave sensibly when the database goes away

What doesn't exist yet: running containers, nodes, scheduling, networking and domains, deployments, authentication, and the MCP interface for agents. See [the full list](#not-built-yet) below.

## Quick start

You'll need Go 1.26+ and Docker (Docker is only used here to run a local PostgreSQL).

```sh
# 1. Pick a database password. Nothing in the repo has one baked in.
export MADICLOUD_DB_PASSWORD="$(openssl rand -hex 24)"

# 2. Start PostgreSQL, set up the schema, and run the server
make db-up
make migrate
make run
```

Then, in another terminal:

```sh
./bin/madicloud app create my-first-app
./bin/madicloud app list
```

You should see something like:

```text
Created application my-first-app
  ID:              5c71310c-ad41-4599-99d5-a3baa0374e27
  Name:            my-first-app
  Desired state:   active
  Observed state:  not tracked (no runtime yet)
  Created:         2026-09-30T21:42:06Z
  Updated:         2026-09-30T21:42:06Z
```

Stop the server with Ctrl-C and the database with `make db-down`. Any PostgreSQL 13 or newer works if you'd rather point at your own instead of using Docker.

## Using the CLI

```sh
madicloud app create <name>
madicloud app list
madicloud app get <id>
madicloud app delete <id>
```

By default the CLI talks to `http://127.0.0.1:8080`. Set `MADICLOUD_API_ADDR` to point it somewhere else.

`create` is safe to retry. If the network drops mid-request, the CLI tries again (up to three times) in a way that can't end up creating the app twice. More on how that works below.

## Using the API

The API lives under `/v1` and speaks JSON:

```sh
# create
curl -H 'Content-Type: application/json' -d '{"name":"billing-api"}' \
     http://127.0.0.1:8080/v1/apps

# list (paginated, sorted by name)
curl 'http://127.0.0.1:8080/v1/apps?limit=50'

# get / delete
curl http://127.0.0.1:8080/v1/apps/<id>
curl -X DELETE http://127.0.0.1:8080/v1/apps/<id>
```

An application looks like this:

```json
{
  "id": "5c71310c-ad41-4599-99d5-a3baa0374e27",
  "name": "billing-api",
  "desired_state": "active",
  "created_at": "2026-09-30T21:42:06.118001Z",
  "updated_at": "2026-09-30T21:42:06.118001Z"
}
```

When something goes wrong, you always get the same shape back: a stable code for programs to check, and a message for people to read.

```json
{"error": "conflict", "message": "an application with this name already exists"}
```

Every response also carries an `X-Request-Id` header that matches the server logs, which makes debugging much easier.

The full reference, with every status code and edge case, is in [docs/api.md](docs/api.md).

### App names

Names end up in URLs and eventually in domain names, so they're strict. They must be 3 to 63 characters of lowercase letters, digits, and hyphens, start with a letter, end with a letter or digit, and can't contain `--`. So `billing-api` and `web2` are fine; `My-App`, `2fast`, and `app-` aren't. Uppercase is rejected rather than quietly lowercased, so there's never any doubt about what an app is called.

The database enforces these rules too, not just the Go code.

### Retrying safely

Networks fail. If you send a create request and never hear back, you don't know whether the app was created. Retrying blindly gets you a "name already exists" error for an app you just made.

To avoid that, send an `Idempotency-Key` header with any unique value:

```sh
curl -H 'Content-Type: application/json' -H 'Idempotency-Key: 3f0c9e1a' \
     -d '{"name":"billing-api"}' http://127.0.0.1:8080/v1/apps
```

If you send the same request with the same key again, you get the original response back (marked with `Idempotent-Replayed: true`) and nothing new is created. This holds even if a dozen retries arrive at once. Reusing a key for a *different* request is an error, failed requests aren't remembered (so retrying them actually retries), and keys are kept for 24 hours. The CLI does all of this for you.

## Desired state vs. what's actually running

This idea shapes the whole design, so it's worth a paragraph.

MadiCloud separates what you **asked for** (desired state) from what's **actually happening** on the machines (observed state). Eventually, background controllers will keep comparing the two and fix any difference: start what's missing, stop what shouldn't be there, restart what crashed.

Today only the first half exists. `desired_state` tells you what you asked for. There's no observed state because nothing is observing anything yet. That's why the CLI says "not tracked" instead of "running".

[docs/architecture.md](docs/architecture.md) goes deeper on how the pieces fit together.

## Health checks

There are two endpoints, and they answer different questions:

- **`/v1/health`**: is the process alive? It never touches the database, so it's what you'd use to decide whether to restart the server.
- **`/v1/ready`**: can it actually do its job? It checks that PostgreSQL is reachable and that the schema is up to date. Use it to decide whether to send traffic.

If PostgreSQL goes down, `/v1/health` stays green, `/v1/ready` goes red, and app requests return a `503` telling you storage is unavailable. When the database comes back, everything recovers on its own. No restart needed.

## Configuration

Everything is configured through environment variables.

The server:

| Variable | Default |
| --- | --- |
| `MADICLOUD_HTTP_ADDR` | `127.0.0.1:8080` |
| `MADICLOUD_DB_HOST` | `localhost` |
| `MADICLOUD_DB_PORT` | `5432` |
| `MADICLOUD_DB_NAME` | `madicloud` |
| `MADICLOUD_DB_USER` | `madicloud` |
| `MADICLOUD_DB_PASSWORD` | *(required)* |
| `MADICLOUD_DB_SSLMODE` | `disable` for localhost, `verify-full` otherwise |

The CLI only needs `MADICLOUD_API_ADDR` (default `http://127.0.0.1:8080`).

The server refuses to start with a bad config and tells you everything that's wrong at once. The database password never shows up in logs, errors, or API responses. Since there's no authentication yet, the server listens on localhost only by default, and it warns you if you change that.

## Database and migrations

PostgreSQL holds MadiCloud's own state: which apps exist, and later, nodes, deployments, and so on. It's *not* a database for your apps. Offering managed Postgres to apps is a separate feature for later.

Schema changes are plain SQL files in `migrations/`, compiled into the binary. `madicloudd migrate` applies them in order, each one in its own transaction, and it's safe to run more than once. The server won't migrate on its own; changing the schema is always a deliberate step. If a migration file is edited after it's been applied, or the database is newer than the binary, MadiCloud notices and refuses to report ready rather than running against a schema it doesn't understand.

If you're writing a migration: never edit one that's already been applied. Add a new file instead.

## Running the tests

```sh
make check              # formatting, go vet, and unit tests (no database needed)
make test-integration   # the real thing, against PostgreSQL
```

The integration tests need a running PostgreSQL and the `MADICLOUD_DB_*` variables set. Each test creates its own throwaway database and cleans up after itself, so your dev data is safe. They cover migrations, concurrency, database constraints, idempotent retries, and the full API end to end, including surviving a server restart.

There's also a fuzz test for the create endpoint:

```sh
go test -run '^$' -fuzz FuzzCreateAppBody -fuzztime 30s ./internal/api/
```

## Not built yet

Requests for these return `404`, not a fake success:

- Actually running apps (containers, runtime, observed state, controllers)
- Editing or renaming apps
- Authentication and permissions
- TLS on the API
- Nodes, scheduling, and resource accounting
- Deployments from Git
- Networking, custom domains, and ingress (Traefik)
- Persistent storage and managed databases for apps
- Preview environments, autoscaling, scale-to-zero
- The MCP interface for AI agents
- Metrics and tracing beyond logs and health checks

## A few design choices

- **One server binary, not a pile of microservices.** It's split into clean packages internally, but deploying it is just running one process.
- **The database is the source of truth.** Rules like "names are unique" are PostgreSQL constraints, so they hold no matter what writes to the database.
- **Honest over impressive.** If a feature doesn't exist, the API says so. Nothing is simulated.
- **Few dependencies.** The only third-party library is the PostgreSQL driver ([pgx](https://github.com/jackc/pgx)). Everything else is the Go standard library.
- **Agents go through the front door.** When the MCP interface arrives, it'll use this same API. It will never touch Docker, the database, or the host directly.

## Project layout

```text
cmd/madicloud/        the CLI
cmd/madicloudd/       the control-plane server
internal/api/         HTTP handlers
internal/application/ the app model, validation, and business rules
internal/client/      Go client for the API (used by the CLI)
internal/config/      environment config
internal/database/    PostgreSQL: connection pool, migrations, storage
internal/version/     version info
migrations/           SQL schema migrations
deployments/          docker-compose for local PostgreSQL
docs/                 API reference and architecture notes
```
