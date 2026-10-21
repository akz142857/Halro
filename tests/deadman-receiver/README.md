# Local deadman receiver fixture

`local_receiver.py` exercises the sender and the
[receiver contract](../../deploy/observability/external-probe/RECEIVER-CONTRACT.md)
on one test host. It requires TLS with a client certificate and a bearer token,
commits received events to a SQLite WAL with full sync, tracks the greatest
sequence per probe, and records heartbeat `firing`/`resolved` decisions. It also
serves synthetic HA root, Prometheus freshness and Alertmanager readiness
responses so a real `halro-deadman` process can be tested without provider calls.

Run `python3 -m unittest discover -s tests/deadman-receiver -p 'test_*.py'`
for the fixture's restart, replay and TTL checks. The command accepts paths to
a private database, a test TLS certificate and key, a client CA and a bearer
token file; `--help` lists its flags. Keep these files outside the repository.
`deadman-local.example.yaml` supplies the matching sender configuration after
its absolute paths and cluster identity are replaced with local test values.

The `notifications` table is local test evidence. This fixture does not send to
an independent contact point or write an immutable audit archive, and a host
failure takes both the fixture and a probe on that host down. It cannot sign
off the production receiver or independent failure domain requirements.
