# golang-index

golang-index is a service which serves a feed of new module versions for private modules hosted on GitHub Enterprise.
More detailed information about the response formats and other details can be found at https://index.golang.org/.

## Standing up postgres

Running the binary requires standing up postgres (tests do not — see below):

```sh
export POSTGRES_USERNAME=postgres
export POSTGRES_PASSWORD=postgres
export POSTGRES_HOST=127.0.0.1
export POSTGRES_PORT=55432 # In case 5432 is in use already.
export POSTGRES_DB=index
docker run \
    -e POSTGRES_USERNAME=$POSTGRES_USERNAME \
    -e POSTGRES_PASSWORD=$POSTGRES_PASSWORD \
    -e POSTGRES_DB=$POSTGRES_DB \
    -p "$POSTGRES_PORT:5432" \
    -d postgres
```

## Running the app

```sh
# Stand up postgres.
go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@latest
migrate -source file://migrations -database "postgres://$POSTGRES_USERNAME:$POSTGRES_PASSWORD@$POSTGRES_HOST:$POSTGRES_PORT/$POSTGRES_DB?sslmode=disable" up

# Authenticate with a personal access token:
go run . -githubHostName=github.mycompany.net -githubAuthToken=...

# ...or with mutual TLS, optionally routing through a proxy that terminates it.
# -githubHostName is still the GitHub Enterprise host used for module paths and
# repo URLs; -githubBaseURL is where requests are actually sent (defaults to
# https://<githubHostName>).
go run . \
    -githubHostName=github.mycompany.net \
    -githubBaseURL=https://gitproxy.mycompany.net \
    -githubTLSClientCertFile=/path/to/client.crt \
    -githubTLSClientKeyFile=/path/to/client.key \
    -githubTLSCACertFile=/path/to/ca.pem
```

## Running tests

Tests need no setup. They start an ephemeral embedded Postgres — a real Postgres
binary, downloaded and cached on first run (no Docker, no standing database) —
and apply the migrations automatically.

```sh
go test ./...
```

To connect to the app's postgres (see above) with psql for debugging:

```sh
PGPASSWORD=$POSTGRES_PASSWORD psql -h $POSTGRES_HOST -p $POSTGRES_PORT -d index -U $POSTGRES_USERNAME
# Tip: List tables with \d. Quit with \q.
```
