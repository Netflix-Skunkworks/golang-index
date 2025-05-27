# golang-index

golang-index is a service which serves a feed of new module versions for private modules hosted on GitHub Enterprise.
More detailed information about the response formats and other details can be found at https://index.golang.org/.

## Running tests

Running tests requires a running Postgres, with migrations run, and providing
environment variables as follows:

```sh
export POSTGRES_USERNAME=postgres
export POSTGRES_PASSWORD=postgres
export POSTGRES_HOST=0.0.0.0
export POSTGRES_PORT=55432 # In case 5432 is in use already.
export POSTGRES_DB=index
docker run \
    -e POSTGRES_USERNAME=$POSTGRES_USERNAME \
    -e POSTGRES_PASSWORD=$POSTGRES_PASSWORD \
    -e POSTGRES_DB=$POSTGRES_DB \
    -p "$POSTGRES_PORT:5432" \
    -d postgres
go test ./... -v
```

Connect to psql for debugging:

```sh
PGPASSWORD=$POSTGRES_PASSWORD psql -h $POSTGRES_HOST -p $POSTGRES_PORT -d index -U $POSTGRES_USERNAME
# Tip: List tables with \d. Quit with \q.
```
