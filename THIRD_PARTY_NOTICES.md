# Third-party notices

Weather Agent's executable includes these Go libraries. Their license texts are
included under `licenses/` in the source and distribution package.

| Library | Version | License |
| --- | --- | --- |
| [Open Location Code](https://github.com/google/open-location-code) | `v0.0.0-20250620134813-83986da0156b` | Apache License 2.0 |
| [Go MySQL Driver](https://github.com/go-sql-driver/mysql) | `v1.10.1` | Mozilla Public License 2.0 |
| [edwards25519](https://filippo.io/edwards25519) | `v1.2.0` | BSD 3-Clause |

Open Location Code implements Plus Code validation, recovery and decoding.
Go MySQL Driver implements MariaDB connectivity and uses edwards25519 for
supported authentication. Refer to the linked upstream projects for source.
