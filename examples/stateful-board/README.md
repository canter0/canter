# Stateful board

This example declares a public HTTP application that depends on a private Postgres capability. The application does not provision Postgres, carry provider credentials, or name an infrastructure vendor. `canter compile` emits the dependency graph, and `canter host bootstrap` publishes a runtime plan that the generic node reconciles through its registered service drivers.

The Postgres driver realizes the database from ordinary host primitives, generates local credentials, waits for readiness, and gives the release a generic `CANTER_SERVICE_DATABASE_URL` binding. Application replacement does not replace the database.

```sh
go build -o bin/canter ./cmd/canter
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/canter-node-linux-amd64 ./cmd/canter-node
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags '-X main.version=v1' -o tmp/stateful-board/app ./examples/stateful-board/cmd/app
go build -o tmp/stateful-board/traffic ./examples/stateful-board/cmd/traffic
tar -C tmp/stateful-board -czf tmp/stateful-board-v1.tar.gz app

./bin/canter compile --file examples/stateful-board/system.yaml
./bin/canter host bootstrap --file examples/stateful-board/system.yaml --node bin/canter-node-linux-amd64
./bin/canter release publish --file examples/stateful-board/system.yaml --artifact tmp/stateful-board-v1.tar.gz
tmp/stateful-board/traffic --target http://SYSTEM_ADDRESS:8080 --duration 4m --users 36
./bin/canter host destroy --file examples/stateful-board/system.yaml --yes
```

The workload creates authenticated users and sessions, then mixes feed reads, transactional post creation, updates, full-text search, logins, identity reads, and likes with per-user think time. It reports status codes, operation counts, throughput, and latency percentiles every 15 seconds.

The recorded live run and its failure-driven lifecycle correction are in [the stateful traffic acceptance record](../../docs/stateful-traffic-acceptance.md).

## Governed schema and release change

The project-archiving migration is intentionally inside Change v0's narrow expand-only language. It adds a nullable column and a partial index using `IF NOT EXISTS`. The application proof endpoint verifies the release environment and actual PostgreSQL schema together.

```sh
./bin/canter change draft \
  --file examples/stateful-board/system.yaml \
  --artifact tmp/stateful-board-v4.tar.gz \
  --summary 'Enable project archiving safely' \
  --migration examples/stateful-board/migrations/project-archiving-v1.sql \
  --migration-id project-archiving-v1 \
  --database database \
  --env ENABLE_ARCHIVING=true \
  --verify /api/change-proof \
  --contains '"ready":true'
```

Drafting does not alter production. Review the emitted plan and operation program, then pass its exact digest to `change authorize`. See the [Change v0 acceptance record](../../docs/change-v0-acceptance.md) for failure and resume behavior.
