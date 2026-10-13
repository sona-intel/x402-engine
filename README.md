# Sona Intel — x402 engine

How Sona analyses x402 payments. It reads the settlements collected by the `indexer` repository, for Monad (`eip155:143`) and Base (`eip155:8453`), and works out:

- which facilitator or relayer settled each payment;
- genuine versus raw volume, with the rules published so anyone can check them;
- seller, payer and facilitator statistics and rankings.

Settings come from the libpq variables and `X402_CONFIG_DIR` (see `.env.example`). Never commit real keys.

## Tests

Integration tests need the stack Postgres (`docker compose up -d x402-postgres` in `services/`) and skip when `X402_TEST_DSN` is unset. From `services/`, in one shell command:

```sh
set -a; . ./.env; set +a; export PGPASSWORD="$X402_PG_PASSWORD" X402_TEST_DSN="host=127.0.0.1 port=5433 user=x402 dbname=x402"
(cd ../x402-engine && go test -p 1 ./... && golangci-lint run)
```

## Run and call

`docker compose up -d --build x402-engine` in `services/` builds and starts it. A changed `config/*.yaml` needs `docker compose up -d --force-recreate x402-engine`. The API is internal (host port 8181, spec in `openapi.yaml`, shown in Swagger):

```sh
curl -s http://127.0.0.1:8181/internal/v1/chains/eip155:143/x402/status
curl -s 'http://127.0.0.1:8181/internal/v1/chains/eip155:143/x402/payments?facilitator=dexter&limit=5'
curl -s http://127.0.0.1:8181/internal/v1/chains/eip155:8453/x402/leaderboards/facilitators
```

Base keeps the payment rows of the last 90 days. A daily job deletes older rows from the engine copy and from the `envio_base` schema, which needs `DELETE` on the indexer tables for `x402_engine` (the `initdb` script grants it for new volumes, and an existing volume needs `alter default privileges for role x402 grant delete on tables to x402_engine` run as `x402` before the Base indexer first starts).
