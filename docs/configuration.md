# Configuration reference

Configure one or more locations in one Weather Agent service. The collector uses
one MariaDB Unix socket, one process lock and a shared 900-second cadence.
Restart after changing configuration; there is no automatic reload.

## Configure locations

Copy `examples/collector.example.json` to `collector.json` and edit its synthetic
locations. Version 2 uses this shape:

```json
{
  "version": 2,
  "locations": [
    {"id":"home","name":"Example west","latitude":40.0,"longitude":-105.0,"timezone":"America/Los_Angeles"},
    {"id":"east","name":"Example east","latitude":41.0,"longitude":-74.0,"timezone":"America/New_York"}
  ],
  "poll_seconds": 900,
  "log_level": "info",
  "mariadb_file": "mariadb.json",
  "state_dir": "/var/lib/weather-agent"
}
```

Each location has these fields:

| Field | Meaning |
| --- | --- |
| `id` | Unique stable ASCII identifier, starting with a letter, up to 32 letters, digits or underscores. |
| `name` | Nonblank display name, up to 128 UTF-8 bytes; stored at registration. |
| `latitude`, `longitude` | Required numeric WGS84 coordinates, within -90–90 and -180–180. |
| `timezone` | Named IANA time zone, such as `America/New_York` or `Asia/Taipei`; host-dependent `Local` is refused. |

The array must contain at least one location. Duplicate IDs, mixed `location` and
`locations` representations, unknown fields and invalid coordinates/zones fail.
Version 1 configs with one `location` object remain readable. They still require
the version 2 database schema: config version and schema version are separate.
Use [the administrative migration](../sql/README.md#upgrade-an-existing-version-1-database)
before replacing an existing deployment.

`poll_seconds` is fixed at `900`. `mariadb_file` references the separate connection
JSON. `state_dir` holds the process lock, not a weather-sample spool. `log_level`
accepts `info` (default) or `debug` in either config version.

On collection startup, missing location/status rows are registered transactionally.
`check` performs no registration or other DML and permits new, unregistered IDs.
Persisted IDs, coordinates and time zones cannot be reassigned. Use a new ID for
a different place; moving stored data requires a reviewed administrative migration.
Removing a config entry stops its polling, preserves stored readings and leaves its
queued backfills pending. MCP lists registered locations, including removed ones;
listing does not imply that a location is still being polled.

## Protect configuration and state

Non-secret collector and connection JSON use `0640`, with legacy `0600` accepted.
Credential files remain `0600`. Ordinary application directories use `0750`, with
legacy `0700` accepted; credential directories may remain `0700`. Use the service
owner/group. Group write/execute and other-user file access are refused. Loading
also rejects symlinks, nonregular files, unknown/duplicate keys, null fields,
invalid UTF-8, trailing JSON, files over 1 MiB and nesting deeper than 32.

References resolve against their containing file. Directory references accept
relative/absolute paths and single/repeated trailing separators. File references
retain trailing-separator refusal. State creation requires an existing safe parent;
it does not create arbitrary ancestors. A second collector fails the process lock.

## Set coordinates from a Plus Code

Use `set-location` before registration to update only one entry's coordinates:

```bash
weather-agent set-location --config collector.json --location home \
  --plus-code 'CWC8+R9 Palo Alto, California'
```

Full codes work offline:

```bash
weather-agent set-location --config collector.json --location home \
  --plus-code '849VCWC8+R9'
```

These codes identify a public location. Replace them with your copied code.
`--location ID` is required when more than one entry exists; it can be omitted
for a single entry, including version 1 configs. Unknown/ambiguous selections
fail without changing the file. Adding entries remains a config edit.

Codes are case-insensitive. A full code ignores any place-name suffix. A short
code sends only its place suffix to Open-Meteo geocoding and requires one exact
city-name match. Missing or ambiguous matches fail. Use a full code when lookup
is ambiguous. Lookup has a 30-second deadline, 1 MiB limit and no redirects.

Review the displayed full code, reference place, cell center and previous
coordinates. Type `yes` to save. Other input, EOF or cancellation preserves the
file. Saving preserves all other values, references, owner, group and file mode;
JSON formatting changes. It checks the original file before an atomic replacement.
Close other editors: this check is not an exclusive writer lock. The containing
directory must permit creating and renaming a temporary file.

No database credentials or writable state directory are needed. Your own config
needs no sudo. For an installed config, run with the service account:

```bash
sudo -u weatheragent /usr/local/bin/weather-agent set-location \
  --config /etc/weather-agent/collector.json --location home \
  --plus-code '849VCWC8+R9'
```

The command does not check stored location identity or migrate data. Collection
still rejects changed coordinates for a registered ID. Plus Codes describe areas;
their centers are not surveyed positions. The existing Open Location Code library
performs decoding and short-code recovery.

## Configure MariaDB credentials

Connection JSON contains `version: 1`, absolute `socket`, `database` and
`credentials_file`. Credentials contain `username` and `password`; whitespace in
passwords is preserved. The collector uses `weatheragent`; MCP uses the distinct
`weather_mcp` account. Provision separate passwords of at least 32 characters.
Keep secrets out of source control, command arguments and diagnostic logs.

`check` verifies the authenticated local identity, exact table grants, schema
version 2 and required InnoDB columns. It executes no configured DML. Collection
uses UTC sessions and strict SQL mode. There is no TCP fallback or runtime DDL.
The collector additionally needs INSERT on `collector_status` for registration.

## Poll locations and request history

Each location has an independent worker, status and retry schedule. All workers
share one SQL pool/HTTP client, at most two active provider requests and at least
one second between request starts across current and historical endpoints.
Waiting current requests take priority over waiting history requests. One slow
request can occupy a slot; it does not serialize all locations.

A valid provider Retry-After pauses all locations. Its maximum deadline is saved
in status rows and restored after restart. A database outage can prevent that
save; the running process still observes its in-memory cooldown. Ordinary errors
and exponential retries remain local to the failed location. Shutdown cancels all
workers. `collect --once` attempts a current sample and at most one eligible
history chunk per location without waiting for deferred retries.

The MCP profile exposes six tools:

| Tool | Input |
| --- | --- |
| `weather__list_locations` | No arguments; returns registered IDs, names, coordinates, time zones and attribution. |
| `weather__get_latest` | `location_id` |
| `weather__get_history` | `location_id`, explicit `source`, UTC `start` and `end`; optional `limit`. |
| `weather__get_status` | `location_id` |
| `weather__request_backfill` | `location_id`, lowercase UUIDv4 `request_id`, `start_date`, `end_date`. |
| `weather__get_backfill` | `request_id`; result includes `location_id`. |

All read tools require `weather:read`; request submission instead requires
`weather:write`. These scopes cover all registered locations; there is no
per-location authorization. Consent remains explicit. Profile updates change
input schemas, so refresh MCP tools after the authorized update.

Backfill dates use the selected location's time zone, start in 1940, are ordered,
span at most 366 inclusive dates and end no later than local yesterday. Queue
acceptance precedes provider validation/retrieval. Requests use seven-local-date
chunks and UTC instants, including DST and fractional time-zone offsets. Hourly
archive samples stay on the provider's UTC hourly grid.

Use the request UUID to read committed progress, retry availability and fixed error
codes. Claims last five minutes and fence location, token and expiry. Retryable
failures have at most five attempts per chunk, with 60/120/240/480-second delays
extended by Retry-After. Failed requests retain prior committed data. After an
uncertain submission, compare location and dates before retrying; intentional new
requests need a new UUID. Historical data never silently replace current data.

## Enable diagnostic logging

Set `"log_level": "debug"` for persistent diagnostics, or override one invocation:

```bash
weather-agent collect --config collector.json --debug
journalctl -u weather-agent -f
```

Logs are structured text on stderr with UTC timestamps. Systemd/journald supplies
storage and rotation; the agent creates no log files. Info includes lifecycle and
recovery summaries; warnings/errors identify throttles and failed work by fixed
codes. Debug includes location ID, current/history operation, HTTP status/duration,
committed inserted/replayed counts, job UUID/attempt and next scheduling times.
A provider response is distinct from an acknowledged database commit.

Logs exclude usernames, passwords, connection strings, full URLs/query strings,
precise coordinates, Plus Codes, raw payloads and SQL arguments. The interactive
setup preview intentionally displays coordinates on stdout. There is no log
hot reload; restart after changing `log_level`.

## Verify an isolated installation

Set `WEATHER_TEST_DATABASE_CONFIG` to a private fixture config with synthetic home
coordinates 40/-105 and `WEATHER_TEST_SOCKET` to a socket under `/build/weather-agent`.
Prepare schema 2 and the restricted fixture accounts explicitly. Fixture passwords
in tests are synthetic and must never be deployed. The mandatory runner does not
create a database or fall back to production credentials.

```bash
python3 tests/private_acceptance.py
python3 tests/test_profile.py
python3 tests/test_package.py
```

`tests/test_migration.py` separately resets only its explicitly selected private
fixture database to test version 1 upgrades. Recreate the empty version 2 fixture
before the general acceptance run to avoid inheriting its test jobs.

The MCP fixture normally builds the independent source under `/build`; set
`MCP_PROJECT` for another checkout or `MCP_BINARY` for an already compiled runtime.
It uses loopback HTTP and temporary OAuth state. Tests use provider responses from
fixtures; they make no live Open-Meteo requests or production changes.
