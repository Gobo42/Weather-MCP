# Weather Agent

Weather Agent collects configured locations' modeled temperature and relative
humidity from Open-Meteo every 15 minutes. A separate MariaDB MCP application reads
stored data and submits historical requests through a MariaDB queue. There is no
agent HTTP listener or message broker.

Weather data are provided by [Open-Meteo](https://open-meteo.com/), under
[CC BY 4.0](https://open-meteo.com/en/licence). Data are modeled conditions, not
measurements from your own weather station. The free API configuration assumes
[noncommercial home-automation use](https://open-meteo.com/en/terms). Current and
hourly archive data remain distinct sources.

## Build and test

Use Go 1.26.2 or later on the development host. Do not build on the deployment host.
Direct dependencies are the MariaDB Go driver and Google's Open Location Code
library. See [third-party notices](THIRD_PARTY_NOTICES.md) for licenses.

```bash
GOCACHE=/build/cache GOMODCACHE=/build/mod GOTMPDIR=/build/tmp TMPDIR=/build/tmp go test ./...
bash scripts/package.sh --output-dir dist
```

The package targets Linux amd64/Debian 13. It contains the compiled binary,
synthetic example configuration, database scripts, MCP templates, documentation,
and a systemd unit. Packaging installs or starts nothing. Existing archive or
checksum filenames are never replaced.

Go tests without `WEATHER_TEST_DATABASE_CONFIG` skip database fixtures. Mandatory
acceptance is separate and fails when an isolated fixture is unavailable; skipped
database tests do not establish acceptance. See [configuration](docs/configuration.md)
for private acceptance requirements.

## Prepare the destination

Provisioning and installation are administrative operations on the deployment
host. Review the SQL and service template before running commands there.

```bash
sha256sum -c weather-agent-linux-amd64.tar.gz.sha256
tar -xzf weather-agent-linux-amd64.tar.gz
cd weather-agent
```

Create a non-root Linux `weatheragent` service account, install the executable
as `/usr/local/bin/weather-agent`, and prepare `/etc/weather-agent` with private
credential files. Create `/var/lib/weather-agent` with ownership `weatheragent`
and mode 0750 before check/one-shot collection. The systemd template also manages
this state directory when explicitly enabled by an administrator.

Follow [database provisioning](sql/README.md), then adapt the example files using
[the configuration reference](docs/configuration.md). Example coordinates are
synthetic. Copy `collector.example.json` to `collector.json`,
`mariadb.example.json` to `mariadb.json`, and the credential example to
`credentials/database.json`. Use mode 0750 for configuration and state directories, 0700 for credential
directories, 0640 for non-secret config files, and 0600 for credentials. Use service-account ownership. Existing
0700 application directories remain compatible; migrate their modes deliberately
rather than applying a recursive chmod.

You can also [set coordinates from a copied Plus Code](docs/configuration.md#set-coordinates-from-a-plus-code)
on the development box before copying the config to the destination:

```bash
weather-agent set-location --config collector.json --location home \
  --plus-code 'CWC8+R9 Palo Alto, California'
```

This command previews the result and asks you to type `yes` before saving. Full
codes decode offline; shortened codes look up their city through Open-Meteo.

```bash
sudo -u weatheragent /usr/local/bin/weather-agent check --config /etc/weather-agent/collector.json
sudo -u weatheragent /usr/local/bin/weather-agent collect --config /etc/weather-agent/collector.json --once
```

Check performs metadata reads only. One-shot collection attempts one current sample
and at most one eligible history chunk per location; it never waits for deferred
retries. Continuous collection uses the same configuration without `--once`.
Enable the included systemd unit only after separately approving installation.

## Connect MariaDB MCP

Install the MCP templates as `apps.d/weather.json`, `connections/weather.json`,
and `credentials/weather.json` under the independent MCP configuration root.
For a source checkout, templates are in `examples/mcp`; distribution templates
are under `config/mcp`. The schema belongs to Weather Agent, not the MCP server.

The six tools list registered locations and expose location-filtered latest
readings, source-specific history, collection status, backfill submission and
progress by UUID. Reads/submissions require `location_id`; UUID lookups return it. The MCP account can SELECT declared
tables and INSERT requests only. It cannot modify readings or worker progress.
Run the independent MCP server's check command before an authorized restart.

Reconnect with `weather:read` for reads and explicitly consent to `weather:write`
for requests. Neither scope implies the other. Keep existing Tapo/Nest/SNMP scopes
when consenting; do not edit stored grants or reset OAuth state to widen access.

## Data and failure boundaries

- Temperatures are Celsius, humidity is percent, and persisted instants are UTC.
- Requests use inclusive calendar dates in the selected location's time zone. DST days retain their real
  23/25-hour lengths. History requests cover at most 366 dates, in seven-date chunks.
- A request INSERT acknowledges queue acceptance, not completed weather retrieval.
  Archive coverage can lag; missing data produce a visible failed request.
- A request needs a caller-generated lowercase UUIDv4. After an uncertain commit
  or duplicate error, look it up and compare its location and dates before retrying.
- Historical retries and checkpoints are durable and fenced. Failed requests
  retain prior committed data; intentionally retry with a new UUID.
- Existing observations are not silently revised. Changed provider values at the
  same observation key produce `observation_conflict` and preserve stored values.
- There is no current-sample disk spool. A database outage can lose snapshots.
  Hourly history does not recreate 15-minute current-condition samples.
- History reads cover at most 31 days. Honor `truncated` and split into smaller
  UTC windows rather than claiming an incomplete result is complete.
- No automatic deletion or retention compaction exists.

Version 2 configuration uses a `locations` array; version 1 single-location input
remains supported. Both require schema 2. Upgrade existing deployments with the
[administrative migration](sql/README.md#upgrade-an-existing-version-1-database)
before replacing the collector and Weather MCP profile.

Workers share provider throttling and a durable global cooldown. Set `log_level`
to `debug`, or use `collect --debug`, for sanitized diagnostics in stderr/journald.
See [configuration and logging](docs/configuration.md) for fields and boundaries.

Live provider acceptance and production deployment require separate verification.
