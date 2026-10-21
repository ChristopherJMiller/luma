#!/usr/bin/env bash
# pg-test: run a command against a throwaway Postgres 16 (unix socket, trust
# auth, fsync off), then stop it. The server has database "luma" and the roles
# "postgres" (superuser) and "luma_conductor"; PGHOST, PGPORT, PGUSER,
# PGDATABASE and PGSSLMODE=disable are exported for the command. Needs initdb
# and pg_ctl on PATH: run inside `nix develop`.
#
#   nix develop -c hack/pg-test.sh psql -c 'select version()'
#
# The Go store tests start their own server (core/internal/store/storetest);
# this script is for ad-hoc work and for tests in other languages.
set -euo pipefail

if [[ $# -eq 0 ]]; then
	echo "usage: hack/pg-test.sh <command> [args...]" >&2
	exit 2
fi
for bin in initdb pg_ctl psql; do
	command -v "$bin" >/dev/null || {
		echo "pg-test: $bin not on PATH (run inside nix develop)" >&2
		exit 1
	}
done

work=$(mktemp -d)
port=$((20000 + RANDOM % 40000))
cleanup() {
	pg_ctl stop -m immediate -D "$work/data" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT

initdb -D "$work/data" -U postgres --auth=trust -E UTF8 --no-sync >/dev/null
pg_ctl start -w -t 60 -D "$work/data" -l "$work/log" \
	-o "-p $port -k $work -c listen_addresses= -c fsync=off" >/dev/null

psql -h "$work" -p "$port" -U postgres -d postgres -v ON_ERROR_STOP=1 -q \
	-c 'create database luma' -c 'create role luma_conductor login'

export PGHOST=$work PGPORT=$port PGUSER=postgres PGDATABASE=luma PGSSLMODE=disable
"$@"
