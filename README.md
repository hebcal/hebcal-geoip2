# hebcal-geoip2

A small, fast GeoIP lookup microservice for [Hebcal](https://www.hebcal.com).

It memory-maps a MaxMind **GeoLite2-City.mmdb** database and answers a single
HTTP endpoint, `GET /lookup?ip=<addr>`, returning the subset of the MaxMind
record that [hebcal-web](https://github.com/hebcal/hebcal-web)'s
`getLocationFromGeoIp()` consumes.

## Why

The Node.js `maxmind` library reads the entire ~63 MB database into the heap
and holds it for the life of the process, even though fewer than ~1 in 500
hebcal.com requests need a GeoIP lookup. This service moves that memory out of
the web process:

- It uses [`maxminddb-golang`](https://github.com/oschwald/maxminddb-golang),
  which **mmaps** the database. Resident memory is only the pages actually
  touched (a few MB), and those pages are shared and reclaimable through the OS
  page cache.
- The web process talks to it over a **Unix domain socket**, so a co-located
  lookup costs ~0.2–0.5 ms instead of holding 63 MB resident.

Measured: ~9 MB RSS for this service vs. ~63 MB of private Buffer in Node, and
sub-millisecond UDS round-trips.

## API

```
GET /lookup?ip=<address>
```

- `200` with a JSON body when the address is found:
  ```json
  {
    "country": {"iso_code": "US"},
    "city": {"geoname_id": 5128581},
    "location": {"latitude": 40.7, "longitude": -74.0, "accuracy_radius": 20, "time_zone": "America/New_York"},
    "postal": {"code": "10001"}
  }
  ```
  (Fields are omitted when absent from the record. The shape matches what the
  Node `maxmind` library returns, so the hebcal-web logic is unchanged.)
- `204 No Content` when the address is valid but not in the database.
- `400` for a missing or malformed `ip`.
- `503` before the database has loaded.

```
GET /healthz   ->  200 "ok"  (or 503 if the database is not loaded)
```

## Running

```bash
go build -o hebcal-geoip2 .

# Unix domain socket (default; recommended when co-located with the web app)
./hebcal-geoip2 -db /var/lib/GeoIP/GeoLite2-City.mmdb \
                -socket /run/hebcal-geoip2/geoip2.sock

# or TCP (e.g. when running on a separate host in the same VPC)
./hebcal-geoip2 -db /var/lib/GeoIP/GeoLite2-City.mmdb -addr 0.0.0.0:8090
```

### Flags

| Flag           | Default                                  | Description                                            |
|----------------|------------------------------------------|--------------------------------------------------------|
| `-db`          | `/var/lib/GeoIP/GeoLite2-City.mmdb`      | Path to the GeoLite2-City database.                    |
| `-socket`      | `/run/hebcal-geoip2/geoip2.sock`         | Unix socket to listen on (ignored if `-addr` is set).  |
| `-addr`        | _(empty)_                                | TCP address to listen on instead of a Unix socket.     |
| `-socket-mode` | `0666`                                   | Permission bits for the Unix socket.                   |

### Reloading the database

The service reopens the `.mmdb` without downtime when it receives `SIGHUP`:

```bash
systemctl reload hebcal-geoip2     # sends SIGHUP
# or
kill -HUP "$(pidof hebcal-geoip2)"
```

#### Reloading automatically after geoipupdate

`geoipupdate` has no post-download hook, so wire the reload up with a systemd
`.path` unit that watches the database file. This is independent of *how* the
file is refreshed — cron, the `geoipupdate.timer`, or a manual run all trigger
it identically, because `geoipupdate` atomically renames the new file into
place and the rename is what the watch observes:

```bash
cp systemd/hebcal-geoip2-reload.path    /etc/systemd/system/
cp systemd/hebcal-geoip2-reload.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now hebcal-geoip2-reload.path
```

The [`.path`](systemd/hebcal-geoip2-reload.path) unit triggers the oneshot
[`.service`](systemd/hebcal-geoip2-reload.service), which runs
`systemctl reload hebcal-geoip2` (→ `SIGHUP`). The oneshot goes inactive
immediately, so the watch re-arms for the next update.

Alternatively, if you drive updates from the `geoipupdate.timer`, add a drop-in
instead of the path watcher:

```ini
# /etc/systemd/system/geoipupdate.service.d/reload-hebcal.conf
[Service]
ExecStartPost=-/bin/systemctl reload hebcal-geoip2.service
```

or, with a cron-driven update, append the reload to the cron command:

```cron
/usr/bin/geoipupdate && systemctl reload hebcal-geoip2
```

## Deploying

See [`systemd/hebcal-geoip2.service`](systemd/hebcal-geoip2.service) for a unit
that runs the service as a sidecar to the web app, with `RuntimeDirectory`
creating `/run/hebcal-geoip2`. A [`Dockerfile`](Dockerfile) is also provided.

The hebcal-web client lives in
[`src/geoipClient.js`](https://github.com/hebcal/hebcal-web/blob/master/src/geoipClient.js)
and reads its socket path from `hebcal.geoip.socket` in `hebcal-dot-com.ini`.
If this service is unreachable, the web app falls back to `{geo:'none'}`.

## License

BSD-2-Clause. This product includes GeoLite2 data created by MaxMind, available
from [https://www.maxmind.com](https://www.maxmind.com).
