# Reconciled HTTP application

This experiment separates application releases from compute lifecycle. Cloud-init installs only `canter-node`. Application binaries are content-addressed in `m1`; the node verifies, extracts, health-checks, proxies, observes, restarts, updates, and rolls them back.

The recorded live run deployed v1 in 4.278 seconds, updated to v2 on the same host in 4.143 seconds, recovered a forced process restart in 2.790 seconds, and rolled back to v1 in 1.793 seconds. A deliberately unhealthy release remained behind the health gate while public v1 continued serving. Full evidence and remaining limitations are in [`docs/reconciled-system-acceptance.md`](../../docs/reconciled-system-acceptance.md).

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/canter-node-linux-amd64 ./cmd/canter-node
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags '-X main.version=v1' -o tmp/reconciled-http/app ./examples/reconciled-http-app/cmd/app
tar -C tmp/reconciled-http -czf tmp/reconciled-http-v1.tar.gz app

./bin/canter host bootstrap --file examples/reconciled-http-app/system.yaml --node bin/canter-node-linux-amd64
./bin/canter release publish --file examples/reconciled-http-app/system.yaml --artifact tmp/reconciled-http-v1.tar.gz
./bin/canter release status --file examples/reconciled-http-app/system.yaml
```

Build and publish another binary with `main.version=v2` to update without replacing the host. `canter release restart` forces a supervised application restart. `canter release rollback --to <version>` selects a retained release. Finally, `canter host destroy --yes` removes the compute host while preserving release history and receipts in `m1`.
