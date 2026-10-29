# Application diagnostics v2

A service exposes `GET {diagnostics.url}?since=&until=`. Both query values are UTC RFC3339. The body is at most 512 KiB. Redirects are not followed.

`schema_version` is `v1` or `v2`. `items` holds at most 100 objects. Each item has `kind`, `name`, and `observed_at` (UTC RFC3339). Status kinds (`component_health`, `dependency_health`, `resource`, `saturation`) require `status` of `healthy`, `degraded`, `unhealthy`, or `unknown`. `recent_error`, `recent_change`, and `slow_operation` require a non-empty `summary`. `summary` is stored at 1024 characters.

`since` on a later successful pull is the previous success minus 5 minutes. A failed pull does not move that success watermark. The same item is one timeline row because its event key includes the service, kind, name, and source time.
