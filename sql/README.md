# Provision the Weather Agent database

Run this one-time administrative bootstrap only on the intended deployment host
after authorizing provisioning. It refuses an existing `weatheragent` database
or either account. DDL is not atomic: stop on the first error, inspect partial
state, and do not rerun with `--force` or delete an existing database to proceed.

Generate distinct collector and MCP passwords of at least 32 characters. Put
session assignments in a private file readable only by the administrator:

```sql
SET @weatheragent_password='COLLECTOR_PASSWORD';
SET @weather_mcp_password='MCP_PASSWORD';
```

Replace both placeholders with separately generated passwords and retain them
in the corresponding protected credential JSON files. Do not pass passwords on
the command line. From this `sql/` directory, feed the private assignments and
bootstrap into the socket-only administrative client:

```bash
cat /root/weather-passwords.sql provision-weatheragent.sql | sudo mariadb --abort-source-on-error --protocol=socket --socket=/run/mysqld/mysqld.sock
```

The script creates five InnoDB tables and schema version 2. The collector gets
SELECT on the migration ledger, SELECT/INSERT on location and measurements, and
SELECT/INSERT/UPDATE on per-location status and SELECT/UPDATE on requests. It cannot submit commands or delete data.
The MCP account gets SELECT on all five tables and INSERT on requests only.
Neither account gets global/schema-wide grants, roles, DDL, or grant authority.

Location metadata is inserted by collection, not `check`. Verify schema/account
checks with the collector and MCP runtime before enabling services. Verify a
separately authorized one-shot reading and MCP access before calling deployment
complete. No production schema or account is created by building the package.


## Upgrade an existing version 1 database

`migrations/001_initial.sql` remains the original schema. The bootstrap applies it
and `002_multiple_locations.sql` for a fresh installation. Runtime never performs
DDL. Version 1 configuration files still need the version 2 database.

Before upgrading an existing deployment:

1. Stop Weather Agent and stop Weather MCP access during maintenance. If you stop
   the shared MCP service, its other apps are briefly unavailable too.
2. Back up the database with an administrative socket client, retain account grants,
   and save the old binary, collector config and MCP profile. Confirm you can restore
   the backup. DDL commits implicitly; this is not a transactional migration.
3. Confirm the original collector's `location.id`. From this `sql/` directory, run
   an administrative client with `--abort-source-on-error` and no `--force`:

```sql
SET @legacy_location_id='ORIGINAL_LOCATION_ID';
SOURCE migrations/002_multiple_locations.sql;
```

Replace `ORIGINAL_LOCATION_ID` with the existing config ID. A populated location
must match exactly. The migration preserves measurement/request identities, maps
legacy status/jobs to that location, removes the singleton slot and adds location
keys/indexes. An untouched empty schema remains empty; startup registers configured
locations. Orphan data, a mismatched ID or partial migration is refused.

4. Install the compiled new collector and version 2 Weather profile together.
   Keep non-secret configs `0640`, credentials `0600`, ordinary directories `0750`
   and credential directories `0700`. Configs can retain their version 1 shape or
   use the new `locations` array. Do not change a registered ID's coordinates/zone.
5. Run the collector and MCP native checks before starting services. Verify
   `MAX(version)=2`, scoped location reads, and separately authorized ingestion.
   Refresh MCP tools because location arguments have changed.

The migration adds only INSERT on `collector_status` to the existing collector
grants. Other account/table permissions stay unchanged. Its ledger advances to 2
only after row-count/mapping checks. On any failure, keep services stopped and
restore the database/grants and matching old runtime/profile from backup. Do not
retry a partially applied DDL migration or downgrade only the executable. Rerunning
on schema 2 is refused. Existing Tapo/Nest/SNMP schemas are outside this migration.
