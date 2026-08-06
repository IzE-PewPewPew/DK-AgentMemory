# Contributing

Thanks for considering it. This project is small on purpose — please read the scope section before opening a large PR.

## Scope

**In scope:** new agent integrations, importers, embedding/LLM providers, bug fixes, docs, tests, platform support.

**Out of scope:** orchestration primitives (actions, leases, routines, sentinels), P2P sync, SaaS multi-tenancy, fine-tuning, UI frameworks in the viewer.

The design goal is a small operational surface. Every feature is one more thing that can break at 2am on someone else's server. Open an issue before building anything large.

## Setup

```bash
git clone https://github.com/IzE-PewPewPew/DK-AgentMemory
cd DK-AgentMemory

make build          # ./bin/dkm
make test           # unit tests; database tests skip without a database
make dev            # Postgres + pgvector in Docker, prints the URL to export

export DKM_TEST_DATABASE_URL='postgres://postgres:test@127.0.0.1:5433/postgres?sslmode=disable'
make test-integration
```

Requires Go 1.25+. Docker and `golangci-lint` are needed for integration tests
and linting respectively, not for building.

## Standards

- `gofmt` and `golangci-lint run` must pass
- pgx with hand-written SQL — no ORM
- Every HTTP response has a JSON body, including errors
- Every endpoint: one golden-path test and one auth-failure test
- Integration tests over unit tests; real Postgres via testcontainers
- Conventional commits: `feat:`, `fix:`, `docs:`, `refactor:`, `test:`, `chore:`

## Adding an agent integration

1. An entry in the `Agents()` registry in `internal/connect/agents.go`: config
   path per OS, detection, and which `Format` its config uses
2. If it needs a schema none of the existing writers produce, a new `Format` and
   a branch in `internal/connect/writers.go`
3. Table entry in `docs/AGENTS.md` with honest capture and injection columns
4. A test using a fixture config that already contains other MCP servers,
   proving merge rather than overwrite, and asserting a second `Connect` leaves
   the file byte-identical

Be accurate in the capability table. Claiming automatic capture for a host
without lifecycle hooks misleads users and they find out the hard way.

## Adding an importer

1. `internal/importers/<name>.go`, returning a preview before it returns records
2. Must support a dry run with a secret report — that is the default posture,
   because people import years of history that may contain credentials
3. Must dedup — re-importing the same input creates nothing new
4. Test fixture with a realistic transcript including a planted fake secret

## PRs

Small and focused. One concern per PR. Include tests. Update docs in the same PR — docs drift is how projects become untrustworthy.

Note breaking changes explicitly in the description.

## Security

Do not open a public issue for vulnerabilities. See [SECURITY.md](SECURITY.md).

## License

Contributions are licensed under Apache-2.0.
