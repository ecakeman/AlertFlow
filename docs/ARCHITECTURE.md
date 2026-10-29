# Architecture & behaviour

How AlertFlow behaves at runtime. It is based on upmonitor. This page covers
what owns which data, how a check becomes an incident, how the dashboard grid
is stored, how navigation and roles work, and which forms exist. Written for
someone (or some agent) about to change this code.

Companion docs: [CONFIGURATION.md](CONFIGURATION.md) (every `config.yaml` field),
[API.md](API.md) (every endpoint), [DEVELOPMENT.md](DEVELOPMENT.md) (how to run it).

---

## 1. Where data lives

Four stores, each with a strict job. Putting data in the wrong one is the most
common design mistake here.

| Store | Path | Holds | Written by |
| --- | --- | --- | --- |
| `config.yaml` | `<config-dir>/config.yaml` | services, app settings, **widget mode + grid layout** | `config.Save` (atomic temp-file + rename) |
| PostgreSQL | `ALERTFLOW_DATABASE_URL` | users, sessions, checks history, `outbox_events`, `service_tls`, **incidents + comments**, **integrations + notification log**, `a2a_adapter_tasks`, `a2a_adapter_task_aliases` | `internal/db/*.go` |
| images | `<config-dir>/images/<id>.webp` | service icons | `internal/image` |
| `state.json` | outside the config dir | which config dir is active | `internal/state` |

**Rule of thumb:** user-authored configuration a human might hand-edit → YAML.
Machine-generated history, anything high-volume, and anything secret → PostgreSQL.
One server process writes the YAML snapshot, so the first version still runs a
single API replica even though the database can accept more connections. The
monitor process reads that same file.

Why the split matters:

- A service is deleted from `config.yaml`, but its incidents stay in PostgreSQL.
  DTO conversion therefore resolves names defensively (`toIncidentDTO` falls back
  to `"(deleted service)"`). Deleting a service explicitly cascades:
  `handleDeleteService` deletes the image, then removes the service from
  `config.yaml`, then deletes checks, the TLS snapshot, timeline rows, runtime
  state, unpublished outbox rows and incidents in one transaction. That
  transaction takes the same service lock as a check or diagnostics write, and
  those writes re-read the file after the lock and skip the insert when the
  service is gone. A failed row delete writes the previous config back. An
  image delete error happens before the config write.
- Integration secrets are deliberately **not** in `config.yaml` — that file is
  exposed verbatim by `GET /api/config` (the raw YAML editor) and is what people
  screenshot and commit.

## 2. Changing config safely (copy-on-write)

Every mutation of `config.yaml` goes through `Server.updateConfig` in
`internal/api/server.go`. Under one write lock it: clones the config → applies
your mutation → validates → saves atomically → swaps the pointer in → re-syncs
the scheduler.

```go
err := s.updateConfig(func(cfg *config.Config) error {
    svc := cfg.Find(id)
    if svc == nil { return errServiceNotFound }
    svc.Name = in.Name
    return nil
})
```

Consequences to respect:

- Never mutate `s.cfg` directly — readers hold it without a lock via `s.config()`.
- If you add a field to `config.Service`, also handle it in `config/clone.go`,
  or the clone will alias the original slice/map and mutations will leak.
- A failed validation leaves the old config in place; nothing is half-applied.
- DB-backed features (incidents, integrations) do **not** use `updateConfig` —
  they query `s.conn()` directly.

## 3. The monitoring loop

`ALERTFLOW_ROLE=monitor` runs `internal/monitor/scheduler.go`. One goroutine per
service, each on its own `interval` ticker. The process re-reads the config
directory every second and `Sync`s. A failed read keeps the last good snapshot.
`server` does not start these loops. `POST /api/services/:id/check` still runs
one `CheckNow` in the API process. Unchanged workers keep running. A changed
URL or interval cancels the old worker and waits until it has left the current
cycle before the new one starts. Removed workers stop the same way (`sameCheck`
decides).

One `check()` does, in order:

1. Take `worker.runMu`, so only one cycle per service runs at a time.
2. Take a snapshot of `svc` and `prev := w.lastStatus` under the worker mutex.
3. `runCycle(...)` — the retry ladder (below). One cycle, one row.
4. **Bail if the worker was cancelled mid-flight** (`parent.Err() != nil`) —
   otherwise a config edit or shutdown would record a bogus `offline`.
5. `RecordCheckAndOutbox(...)` — in one transaction, append the check (stamped with the cycle's **start**), an unpublished `monitor.check.completed` outbox row whose payload carries that check id, and the cert snapshot when this cycle has one. The cert write uses the same service lock and presence check. If this fails, stop: do not move `lastStatus`.
6. Update `w.lastStatus`, which the next cycle uses only to decide whether to retry.

The scheduler stops there. It does not call `OnTransition` and it does not send notifications. `published_at` stays NULL in the monitor process. Claiming an outbox row uses `FOR UPDATE SKIP LOCKED` on `outbox_events` only. That is not a lock on a service. `RecordCheckAndOutbox`, diagnostics apply and `DeleteServiceRecords` take `pg_advisory_xact_lock` keyed by the service id. After that lock, a check or diagnostics write re-reads `config.yaml` and skips the insert when the service is gone. When `REDIS_URL` is set, the monitor process invalidates that service's cached reads after a check commits. A Redis failure is logged and the check still writes.

When `ALERTFLOW_K8S_ENABLED` is `true` or `1`, the same process runs `internal/observe/kubernetes`. It watches Pods, Events, ReplicaSets, and Deployments in namespaces named by a service's `kubernetes` block, and maps a Pod to a Deployment through the ReplicaSet owner UID. The initial list is reconciled with `live=false`. Objects added after that sync are handled by the informer's add event; the first observation is still `first_seen`. A semantic fingerprint change writes `runtime_events` with source `kubernetes` and updates `runtime_object_states`. A later event key includes the object's `resourceVersion`, so a return to an earlier fingerprint is stored once for that version and a replay of the same version is ignored. A fingerprint that differs only after a restart, or after a watch expiry relist, is `reconciled_after_gap`. The same fingerprint as the stored row writes nothing. A watch error on any of the four informers marks the source `gap` or `unavailable`, then the cache is reconciled again. Removing or moving a `kubernetes` block marks that mapping's objects inactive and the source `disabled` with `mapping removed`, and leaves the timeline rows in place. Turning collection off does the same for the current mappings, with `collection disabled`, and does not write that row again while the mapping is unchanged. Two services cannot name the same cluster, namespace, and workload. The first list/watch waits at most 15 seconds, off the config reload loop. If it does not finish, the source is marked `unavailable` and the informer keeps retrying. `Close` cancels that wait. It does not call `event.Decide` and it does not open an incident. A connection failure, permission error, or watch expiry updates `runtime_source_state` only after the service lock, and only when the service is still configured. HTTP checks continue. Other Kubernetes writes use the same lock and skip the insert when the service is gone.

`GET /api/services/:id/timeline` reads `runtime_events` through `query`, newest first, with a keyset cursor. The window cannot extend past `ALERTFLOW_TIMELINE_RETENTION_DAYS`. Coverage comes from `runtime_source_state` plus the service's diagnostics and kubernetes blocks. `title` and `summary` are returned at 1024 characters. The response is not cached. A failure on that card does not hide metrics, the certificate, or the ping console. The gRPC `GetTimeline` and `GetSourceCoverage` calls check the RPC context before and after that read. `ServiceTimeline` does not take a context, so a cancellation does not stop SQL that has already started. `GetKubernetesState` does not use the RPC context. Those reads are not cancelled at the database.

The monitor process also runs `internal/diagnostics`, which polls a service's optional `diagnostics.url` on its own interval. Kinds match the agent: `component_health`, `dependency_health`, `resource`, `saturation`, `recent_error`, `recent_change`, and `slow_operation`. A diagnostic `summary` is stored at 1024 characters. An item with `event_id` uses the timeline key `diagnostics:<len(service id)>:<service id>:<event id>`, so the same id on two services stays two rows. A row already stored for that service as `diagnostics:<event id>`, or `diagnostics:<event id>:recovered` for a recovery, is the same event and is not written again. An item without `event_id` uses a sha256 of the service id and the normalized item, including attributes and detail. The first four require `status` (`healthy`, `degraded`, `unhealthy`, or `unknown`). `degraded` is severity `warning` and `unhealthy` is `error`; `healthy` and `unknown` do not write a row. A later `healthy` or `unknown` after `degraded` or `unhealthy` writes one recovery row at severity `info`. `recent_error` is `error`, `slow_operation` is `warning`, and `recent_change` is `info`. A failed pull updates `runtime_source_state` only. Replacing or removing a collector waits until the previous loop has left its current poll. It does not call `event.Decide` and it does not open an incident. The agent still reads its own diagnostics file directly.

`ALERTFLOW_ROLE=publisher` is a second process of the same binary. It claims one due outbox row at a time, in `id` order, and writes it to the Kafka topic `alert-events` with the service id as the key. `published_at` is set only after the broker ack, and only when that process still holds the lease. The publish transaction takes the same service advisory lock as service deletion before it reads the outbox row. It keeps that lock until `WriteMessages` returns, then stores `published_at` in the same transaction. An event that carries `epoch` is skipped when that epoch is not newer than the retired one. An older event, with no `epoch`, is skipped when `occurred_at` is at or before `retired_at`. A delete that already committed removes the unpublished row, so a publisher still holding the old record does not send it. Deleting a service records the life that ended and starts the next one, so a check in the same Unix second as the delete still applies after the service is added again. Run one publisher. PostgreSQL and Kafka still do not commit together: a broker that accepted the message while the client saw a timeout can leave a duplicate for the consumer.

`ALERTFLOW_ROLE=consumer` reads that topic. One message updates the incident, `processed_events`, `service_runtime_state`, and `pending` `notification_deliveries` in one PostgreSQL transaction. That transaction takes the service lock and, when the event's `epoch` is not newer than `retired_services.epoch` (or, when the payload has no `epoch`, when it is not newer than `retired_at`), only records `processed_events`. It does not recreate incidents or runtime state. A check recorded after the service is added again carries the next epoch and is applied. The Kafka offset is committed after that transaction commits. This process does not call `Dispatcher.Notify`. More consumer replicas than the topic's three partitions do not add parallelism.

The default webhook body keeps `status` as `down`, `recovered`, or `warning` and adds `schema_version`, `event_id`, `service_id`, and the occurrence timestamps. `event_id` is `<event>:<incident_id>` when the snapshot has an incident, otherwise `delivery:<notification_deliveries.id>`. `check_id` is copied from the check event. Agent reads and callbacks use gRPC. An unknown `/internal/` or `/webhooks/` path returns JSON, not the SPA.

### The diagnostic agent's two edges

The agent is a separate service. It has two gRPC edges, and neither is a notification channel. An empty `ALERTFLOW_AGENT_GRPC_TARGET` stores the incident and timeline and does not insert an agent task. A set target that cannot be reached stays on the delivery lease and is retried. Optional Prometheus and Loki MCP reads stay in that run's evidence. An empty URL does not open a client. A service without a selector mapping is not queried. A failed read does not write `runtime_events`.

**AlertFlow → agent.** `notification_deliveries` rows with `integration_id` NULL. `notify.AgentSender` encodes the v2 envelope at send time, and delivery calls `StartInvestigation` or `StartChat`. The diagnostic context is not written into the outbox.

**agent → AlertFlow.** `SubmitAgentCallback` on `AlertFlowControlService`, with bearer `ALERTFLOW_AGENT_GRPC_TOKEN`. `message_id` is the idempotency key. A storage failure is a retryable gRPC error. Results land in `agent_runs` (and `agent_chat_requests` for chat), and `/api/incidents/{id}/agent-report` reads that local copy.

Chat uses the same durable path: a question is stored in `agent_chat_requests` together with a `chat_requested` delivery row and answered asynchronously. The agent keeps the matching `chat_requests` row, and enqueues the answer into its own callback outbox **in the same transaction** that marks the question answered, so "answered but never sent" is not a reachable state.

`ALERTFLOW_ROLE=delivery` claims one internal agent task at a time (`notification_deliveries.integration_id` IS NULL) and calls gRPC `StartInvestigation` or `StartChat`. It does not claim historical channel rows and does not fan out with `Dispatcher.Notify`. An unset `ALERTFLOW_AGENT_GRPC_TARGET` leaves those tasks pending. Retired channels are not claimed, retried, or swept to `dead`. The lease is longer than the send timeout and is not renewed. Success, retry, and dead updates require the same lease. A crash after the remote service accepts the request and before PostgreSQL records `succeeded` can send the task again. Delivery is at-least-once, not exactly-once. If the worker keeps crashing before it finishes, the task still stops at the attempt limit: the next claim marks that expired `agent_webhook` row `dead`, clears the lease, and does not write a `notification_log` row. `ALERTFLOW_ROLE=retire-deliveries` runs once and exits. It cancels `pending` and `retry_wait` on the four retired channels and writes `retired_delivery_audits` for `running` rows, which stay `running`. It is not part of server startup. HTTP 408, 425, and 429 wait and retry; other 4xx responses are permanent. A warning delivery can point at its warning incident, but the `notification_log` row for `warning` keeps `incident_id` NULL. Stored and returned errors do not include channel secrets. The test-notification API still calls `Dispatcher.Test` and does not enqueue a task.

### The retry ladder

`runCycle` performs up to `check.retry_attempts` requests, waiting
`check.retry_delays[i]` between them (the last delay repeats; N attempts use N-1
gaps). It classifies the cycle:

| Outcome | Stored as |
| --- | --- |
| attempt 1 succeeded | `online`, `attempts = 1` |
| a later attempt succeeded | `warning` — latency/code from the winning attempt, `error` from the **first** one |
| every attempt failed | `offline` with the last attempt's detail |

`Check()` itself still returns **only `online` or `offline`** and stays a
single-attempt classifier; `warning` is a property of the cycle, decided one
level up. `unknown` remains purely a "no data yet" state in the DB/DTO layer and
never comes from a check.

Three rules keep the ladder from doing harm:

- **No retries while already offline.** Retries exist to decide whether to *open*
  an incident, and one is already open; this also stops a week-long outage from
  tripling the traffic aimed at a dead endpoint.
- **A cycle may not outrun its interval.** Before each retry, `runCycle` checks
  `elapsed + delay + timeout` against the interval and stops early if it would
  overrun. `startWorker` logs a warning when a ladder cannot fit at all — the
  config is never silently rewritten.
- **`sleepCtx`, never `time.Sleep`.** `Stop()` and `Sync()` must not block for
  the length of a ladder.

Rows are stamped with the cycle's **start** rather than its end: an outage began
when the first attempt failed, and it keeps rows interval-aligned, which is what
`chooseBucket` assumes.

Concurrency: `worker.svc` and `worker.lastStatus` are guarded by `worker.mu`,
because the ticker goroutine, `Sync` and a manual `CheckNow` can all touch them.
`worker.runMu` serialises whole cycles on that worker — a ladder widens the
ticker/`CheckNow` overlap enough that the slower one could otherwise act on a
stale `prev`. `CheckNow` uses this lock when the service already has a worker.
If it does not, `CheckNow` builds a temporary worker; there is no ticker to
overlap. The workers map is guarded by `Scheduler.mu`. One ongoing incident per
service is still the partial unique index, not `runMu`. `lastStatus` moves only
after the check transaction commits.

## 4. Incident lifecycle

**The principle: an incident is derived from a check transition, never from a stored status by itself.** The check scheduler does not apply that change. It only stores the check and an outbox event. The consumer applies check transitions with `event.Decide` inside one database transaction. It does not call `OnTransition`. When that transaction actually opens or resolves an outage, it also writes one `runtime_events` row (`incident:{id}:opened` or `incident:{id}:resolved`). A warning check and a repeated open do not. **An incident stays open while the HTTP status is `offline`, and closes when a check recovers.** `warning` still counts as HTTP up. HTTP is the only source that opens or closes an incident. A manual incident is written by `POST /api/incidents` in its own transaction, with the same timeline keys and one `incident_start` task per usable `agent_webhook`.

| prev ↓ / current → | `online` | `warning` | `offline` |
| --- | --- | --- | --- |
| `online` | no-op | no ongoing incident; fires a `warning` notification (opted-in channels only) and logs a `CreateWarningEvent` | `CreateIncident` → `incident_start` |
| `warning` | no-op, no notification | no-op — this is what dedupes a service that blips every cycle | `CreateIncident` → `incident_start` |
| `offline` | `ResolveOngoingIncident` → `incident_resolve` | same, and **no** warning notification or event log: the resolve already says we are back | no-op — why a service that stays down doesn't spawn an incident every tick |
| `unknown` | resolve (silent) | logs a `CreateWarningEvent` (unreachable in practice — `InitialStatus` never seeds `unknown`) | `CreateIncident` → `incident_start` |

A resolving transition closes the service's ongoing incident; there is no second source that can keep it open.

### Incident severity: outage vs. warning

Every incident row carries a `severity`: `outage` (the table above, subject to
the one-ongoing-per-service invariant below) or `warning` (a momentary
retry-recovery event). `CreateWarningEvent` records a warning severity incident
that is **already `resolved`, with `startedAt == resolvedAt`** — it never has an
"open" phase, so it can never collide with the one-ongoing-incident index. This
is what makes a service's "Recent incidents" (and the `/incidents` list) show
warnings alongside real outages without blurring the two: a UI reading
`severity` can style/filter them apart, while `status`/`source` keep meaning
exactly what they meant before this field existed.

`GET /api/incidents` accepts `?severity=` for exactly this reason:
`ServiceDetailView.vue` fetches its chart's outage bands with `severity:
'outage'` and its "Recent incidents" card (mixed severity, small limit)
separately — a service that warns often would otherwise crowd real, older
outages out of a single shared, necessarily-bounded page before the chart
ever saw them.

The consumer takes the previous status from `service_runtime_state`, locked with
`SELECT ... FOR UPDATE`, not from the newest check. A missing row uses the latest
check with a smaller id, or `online` when that history is gone. If an ongoing
incident exists and the state row is missing, the previous status is `offline`,
so a later recovery still resolves it. An event with an older `check_id` than
the state row is stored in `processed_events` and does not move the incident or
the status. A repeated `event_id` is the same no-op. An unreadable message, or
one whose `schema_version` is not 1, is written to `dead_letter_events` and then
its offset is committed. A database error rolls the transaction back and leaves
the offset uncommitted.

Enabled `agent_webhook` integrations receive a `notification_deliveries` row in status `pending`. Telegram, Slack, email and generic webhook do not.
`warning` rows go only to integrations with `notify_warnings`. Recovery from
`offline` to `warning` queues a resolve task and no warning task. The payload is
a snapshot of the event and does not copy integration secrets. Nothing in this
phase calls a sender.

`notification_deliveries.integration_id` is `ON DELETE CASCADE`, same as
`notification_log`. Deleting a channel, or replacing the channel set from an
archive, removes the delivery tasks for the deleted rows. Those rows are not
marked sent. `ReplaceIntegrations` does the delete, the insert, and the identity
reset in one transaction. An archive that has no `integrations.json` does not
call it, so existing tasks stay.

`prev == current` returns immediately, which is the overwhelmingly common path.
`InitialStatus` deliberately still returns only `online`/`offline` — a third seed
value would change nothing in the matrix. The one visible consequence: a restart
that lands exactly on a warning re-seeds as `online` and may send one duplicate
warning notification. The UI is unaffected (status comes from the latest row).

### The invariant: one open incident per service

Enforced in the schema, not in Go. Upstream SQLite defined this in
`00003_incidents.sql`. The current PostgreSQL database is created by one
migration, `migrations/00001_init.sql`:

```sql
CREATE UNIQUE INDEX idx_incidents_one_ongoing ON incidents (service_id) WHERE status = 'ongoing';
```

Any number of `resolved` rows, at most one `ongoing` per service. The check
scheduler does not insert incidents. The consumer does, and a second ongoing
row still hits this index. `CreateIncident` returns `db.ErrOngoingExists` in
that case. Manual creation surfaces it as a `400`. Manual create, edit, and
resolve stay on their existing handlers.

### Surviving a restart

`lastStatus` lives in memory. A new worker seeds it from the newest `checks` row
(`LatestCheckStatus`), or `online` when that service has no row yet. The value
only tells `runCycle` whether to skip retries. It is not an incident decision,
and a restart does not resolve or open an incident.

### Manual incidents

Same table, `source = 'manual'` — for what the monitor can't see (planned
maintenance). Subject to the same one-open invariant. Admins can also edit
timestamps/title, resolve, and delete. Comments live in `incident_comments`
(`ON DELETE CASCADE`), author resolved by joining `users`; **any signed-in user
may comment**, all other mutations are admin-only.

### Retention asymmetry

The hourly retention loop trims **`checks` only** (plus expired sessions).
Incidents are never aged out — outage history outlives the metrics window. They
die only with their service, or by explicit delete.

### Flap protection, and what it does not cover

A single failed check no longer opens an incident: the retry ladder (§3) has to
exhaust every attempt first, so one network blip becomes a `warning` rather than
an outage. `internal/incident` did not need to learn about retries — it is
decoupled from *how* we decided the service is down, and only gained the third
status.

What is still unprotected is **warning notification noise**. A service
alternating `warning → online → warning` fires a warning every other cycle, since
the `prev == current` dedupe does not catch an alternation. That is why warnings
are opt-in per channel. A cooldown would have to live on `worker`; `incident` is
deliberately stateless.

### Why `incident` is its own package

The check scheduler does not import `incident`. `incident` still must not import
`monitor`. Hence everything arrives as arguments:
`OnTransition(ctx, db, svc, prev, Outcome{...}, ts)` — `Outcome`
carries the attempt count and first-failure reason. It depends only on `db`.

## 5. Notifications

Three events: incident start, incident resolve, and `warning` (a check that
recovered on a retry). Nothing else notifies.

`incident.OnTransition` still records an incident when called, and it no longer fans notifications out. The check path and the consumer do not call it. `Dispatcher.Test` remains the immediate "send test" API and only reaches a registered sender. The only registered sender is `agent_webhook`. Automatic delivery is the `delivery` role: one `notification_deliveries` row, one `Sender`, then a `notification_log` row for that attempt.

Warnings are opt-in per channel and off by default: they are frequent by design,
and a channel that pages a human wants outages only. A warning has no incident,
so its log row carries `incident_id = NULL`. Upstream SQLite migration `00005`
rebuilt `notification_log` because SQLite could not alter that `CHECK` or
`NOT NULL`. The current PostgreSQL schema declares the nullable column and the
`warning` event in `migrations/00001_init.sql`; it does not replay that rebuild.

The agent sender is registered at startup because it needs the query layer. Telegram, Slack, email and generic webhook no longer have senders.

### Secret handling

`secretFields` (in `handlers_integrations.go`) lists sensitive keys per type
(`botToken`, `webhookUrl`, `password`). Three rules:

1. **Never echoed.** `toIntegrationDTO` strips them and exposes only
   `secrets: { botToken: true }` so the UI can show a "configured" placeholder.
2. **Merge-on-omit.** On `PUT`, a blank/absent secret keeps the stored value —
   the client can't resend what it can't read.
3. **Included in exports.** A backup without secrets can't restore working
   integrations; the export endpoint is already admin-only.

## 6. Dashboard grid & layout persistence

The grid is `grid-layout-plus` in `ServiceGrid.vue`: 12 columns, row height 68px,
responsive breakpoints `lg/md/sm/xs/xxs` at `1200/900/640/480/0`.

**There is exactly one layout, stored per service in `config.yaml`** as
`layout: {x, y, w, h}` — *not* one layout per breakpoint:

```yaml
layout: { x: 0, y: 0, w: 3, h: 4 }
```

(The `y` key serialises quoted — `"y"` — because yaml.v3 reads bare `y` as a
boolean. That's correct and round-trips; don't "fix" it.)

**The trap:** persist only on a real user drag/resize — `GridItem`'s `@moved` /
`@resized`. Never persist `GridLayout`'s `@layout-updated`, because it *also*
fires when the grid reflows itself for a narrower breakpoint. Wiring saves to it
means opening the app on a laptop silently rewrites the stored desktop layout.
(This was a real bug; the whole arrangement collapsed to `w:2` after one window
resize.)

Corollary that still stands: dragging cards *while on a narrow screen* saves
those narrow coordinates as the one true layout. Fixing that properly needs
per-breakpoint layouts — a schema change.

Saving flows `ServiceGrid` → `services.saveLayout()` → `PATCH /api/services/layout`
(bulk, `[{id,x,y,w,h,mode?,chart?}]`). The same endpoint carries `mode` and
`chart`, which is why `services.setWidgetMode()` / `setChartType()` can reuse it.
Both are only applied server-side when non-empty, so a drag-save doesn't reset
them — but `x/y/w/h` are assigned unconditionally, so those two actions must
re-send the service's current position or they'd zero the layout.

**Dragging must not select text.** interact.js (under `grid-layout-plus`)
deliberately skips `preventDefault` on pointerdown and expects CSS to suppress
selection; the library's own `user-select: none` only lands on
`.vgl-item--dragging`, by which point the browser has anchored a selection — and
never on the neighbours the pointer sweeps across. `ServiceGrid` therefore sets
`select-none` on the whole grid whenever it's interactive (not in `readonly`,
where nothing is draggable and text stays selectable).

### Widget modes

`widget.mode` per service: `icon` (icon + status badge), `name` (icon + name +
URL), `dashboard` (mini dashboard with status pill, response, uptime, sparkline).
Rendered by the three branches of `ServiceCard.vue`.

Card interaction model — three distinct targets, don't conflate them:

- **card body click** → in-app detail page (`/services/:id`), gated on the
  `linkable` prop, with a 4px drag threshold so ending a drag doesn't navigate;
- **↗ hover button** → the external monitored site (available to read-only users
  too);
- **⋯ hover menu** → open / widget mode / chart style / edit / replace image /
  generate icon / delete (admin only).

Because the actions overlay the top-right corner, `dashboard` mode reserves
horizontal room (`headerPad`) so a long name truncates *before* reaching them.
`name` mode needs no reserve — its content is vertically centred, clear of the
buttons — and lets the name wrap to two lines (`line-clamp-2`) instead of
truncating.

### Configurable stat tiles: Warning period and Avg uptime scope

The five stat tiles above the grid ("Services", "Online", "Offline",
"Warning", "Avg uptime") are mostly a live read of `GET /api/services`. Two are
admin-configurable, each via a pencil that fades in on hover
(`opacity-0 group-hover:opacity-100`, so it never appears for a read-only
viewer) and opens a small dialog:

- **"Warning"** counts *distinct services that logged a warning within
  `settings.dashboard.warning_period_hours`* (default 24h) — not, as the other
  tiles are, each service's current live status. That needs its own query over
  a window no other endpoint computes, hence `GET /api/dashboard/stats`
  (`db.WarningServiceCountSince`) polled alongside the services list.
  `services.warningCount` (the *current*-status count) is untouched and still
  drives the sidebar badge — the two numbers can legitimately differ.
- **"Avg uptime"** averages `service.uptime` over every service *except* those
  listed in `settings.dashboard.uptime_excluded_services`. This one needs no
  new endpoint: `uptime` is already on every service from `GET /api/services`,
  so the filtering is a plain client-side computed
  (`stores/services.ts`'s `avgUptime`, which reaches into the settings store
  for the excluded-id list) — reused nowhere else, so it isn't worth a backend
  round trip.

Both dialogs write through `settings.update()`, which always resends the whole
`dashboard` settings object (not just the changed key) — the same
merge-then-PUT shape every other settings field already uses. Backend-side,
`handleUpdateSettings` treats a present-but-empty
`uptimeExcludedServices` array as "include everyone again" and a
present-but-empty/absent one as "leave alone" (mirroring `Check.RetryDelays`'s
existing nil-vs-empty convention), and `Validate` clamps a bad
`warningPeriodHours` back into `[1, maxWarningPeriodHours]` rather than
rejecting the request.

## 7. Navigation, roles & guards

Two roles only: `admin` and `readonly` (a CHECK constraint on `users.role`).

| Path | Name | Access |
| --- | --- | --- |
| `/` | `dashboard` | any signed-in user |
| `/services/:id` | `service-detail` | any signed-in user |
| `/incidents` | `incidents` | any signed-in user (**anyone may comment**) |
| `/incidents/:id` | `incident-detail` | any signed-in user |
| `/resources` | `resources` | **admin** |
| `/integrations` | `integrations` | **admin** |
| `/settings` | `settings` | **admin** |
| `/setup` | `setup` | first-run only (`meta.bare`) |
| `/login` | `login` | anonymous (`meta.bare`) |
| `/:pathMatch(.*)*` | `not-found` | — (`meta.bare`) |

`meta.bare` renders without the sidebar/topbar shell.

The guard order in `router/index.ts` matters: bootstrap auth → force `/setup` if
`needsSetup` → bounce `/setup` once set up → force `/login` if anonymous →
bounce `/login` if signed in → block `readonly` from `adminOnly`.

**Adding a page needs three edits in sync:** the route, the `adminOnly` set (if
restricted), and the nav entry in `AppSidebar.vue`. Route-level gating is UX
only — **the API must enforce it independently** via `auth, admin` middleware.

`/incidents` being open to `readonly` while its mutations are admin-gated is a
deliberate split: the guard lets everyone in, and the page hides
create/edit/resolve/delete behind `auth.isAdmin`.

## 8. Forms (what exists to fill in)

| Form | File | Submits |
| --- | --- | --- |
| First-run setup | `SetupView.vue` | `POST /api/setup` — first admin |
| Sign in | `LoginView.vue` | `POST /api/auth/login` |
| Add/edit service | `services/ServiceFormDialog.vue` | name, url, interval, widget mode, retry attempts/delays → `POST`/`PUT /api/services` |
| Generate icon | `services/IconGeneratorDialog.vue` | procedural SVG → rasterised → icon upload |
| Create/edit incident | `incidents/IncidentFormDialog.vue` | service, title, startedAt, resolvedAt (`datetime-local` ⇄ RFC3339) |
| Comment composer | `IncidentDetailView.vue` | `POST /api/incidents/:id/comments` |
| Add/edit integration | `integrations/IntegrationFormDialog.vue` | type-conditional fields; secrets blank = keep; `notifyWarnings` switch (plain overwrite — the row toggle in `IntegrationsView.vue` must resend it) |
| Settings | `SettingsView.vue` | settings incl. global retry defaults, users, config path, raw `config.yaml` editor |
| Confirm | `common/ConfirmDialog.vue` | generic destructive confirmation |

Dialog convention: `props { open, <entity>? }`, emits `update:open` + `submit`,
reset state on open via `watch(() => props.open, …)`, and let the **parent** call
the store and toast. Presence of `<entity>` means edit mode.

## 9. Images

Optimisation is **client-side on purpose** (`src/lib/image.ts`, Canvas → WebP):
the backend only checks magic bytes and stores, which keeps the binary CGO-free.
Do not add server-side image codecs.

Uploads are a **raw `image/webp` body**, not multipart. Three entry points, all
converging on the same `optimizeToWebP → services.uploadIcon` pipeline: the ⋯
menu's file picker, drag-and-drop onto a card, and Ctrl+V paste (targets the
hovered card via `hoveredServiceId`; ignored while a text field is focused).

With no icon, `ServiceIcon.vue` renders a seeded procedural SVG
(`src/lib/icon-generator.ts`) — deliberately no AI model.

## 10. TLS certificate info

Read from the TLS handshake the HTTPS check already performs
(`resp.TLS.PeerCertificates[0]`) — no extra connection, no WHOIS/RDAP (domain
registration expiry is explicitly out of scope).

Stored as **one upserted row per service** (`service_tls`), not per check: a cert
is stable for weeks, so per-check rows would be pure redundancy.

A failed handshake means there's no `resp` at all, so no cert can be read; the
check error is stored instead (with zero timestamps) so the UI can still show
that something is wrong. Plain-HTTP services are skipped entirely and the detail
page shows "Not applicable".

## 11. Export / import

`GET /api/config/export` zips `config.yaml` + `images/` + `incidents.json` +
`integrations.json`. Import replaces all of it, after snapshotting the current
state to `backups/`.

Two behaviours worth knowing:

- **Missing bundles are skipped, not treated as empty.** An older archive without
  `incidents.json` leaves existing incidents alone rather than wiping them.
- **IDs are preserved** so `incident_comments.incident_id` stays valid, and user
  references that don't exist in the target DB are nulled (users aren't part of
  the archive), so foreign keys hold across instances.
- **New fields ride along without a format change.** Retry settings live in
  `config.yaml`, and `notify_warnings` is a field of the exported
  `db.Integration`. `config.Parse` does not use `KnownFields`, so an older binary
  reading a newer archive ignores what it doesn't know; a newer binary reading an
  older archive gets the Go zero value, which keeps warnings opted **out**.

## 12. Where to make a change

| Goal | Touch |
| --- | --- |
| New API endpoint | route in `api/server.go` → handler in `api/handlers_*.go` → DTO in `api/dto.go` → client in `web-ui/src/api/index.ts` → type in `web-ui/src/types/index.ts` → document in [API.md](API.md) |
| New DB table/column | new `db/migrations/NNNNN_*.sql` (never edit an applied one) → queries in `db/<table>.go` |
| New config field | `config/config.go` struct + `Default()` + `normalize()` + `Validate()` → `config/clone.go` **only if the field is a reference type** (slice/map/pointer — value types ride along in `Clone`'s `copy`) → [CONFIGURATION.md](CONFIGURATION.md) |
| New per-service preference | follow `widget.mode` / `chart.type`: config field → `serviceDTO` → `layoutItem` + an `if it.X != ""` guard in `handleUpdateLayout` → a store action that re-sends the current `x/y/w/h` (the handler assigns layout unconditionally) → card ⋯ menu |
| Change the response-time chart | `services/ResponseTimeChart.vue` (detail) / `dashboard/SparklineChart.vue` (card) — both hand-rolled SVG, no chart library. Bucketing is server-side: `api.chooseBucket` + `db.SeriesFor` |
| Change the ping console | `services/PingConsole.vue` — collapsing runs of successes, and paging 10 collapsed lines at a time, are both client-side on purpose (fetches larger raw batches and pages the squashed result); `db.ListChecks` returns raw rows |
| Change monitoring/incident behaviour | `monitor/scheduler.go` (when — including `runCycle`'s retry ladder) / `incident/incident.go` (what) |
| Change the incidents list/pagination | `IncidentsView.vue` (page-number UI, via `common/PagerControls.vue`) → `stores/incidents.ts` (`INCIDENTS_PAGE_SIZE`) → `handleListIncidents` (`?limit=&offset=`, returns `{ incidents, total }`) → `db.ListIncidents`/`CountIncidents` |
| Change a dashboard stat tile's configurability | `DashboardView.vue`'s hover-pencil buttons → `dashboard/WarningPeriodDialog.vue` / `dashboard/UptimeServicesDialog.vue` → `settings.update({ dashboard: {...} })` → `config.DashboardSettings` (`config.yaml` `settings.dashboard`) |
| Add a check status | `db/checks.go` `CHECK` constraint (new migration) + every `IN ('online','warning')` aggregate → `ServiceStatus` in `web-ui/src/types` → `statusLabel`, `StatusDot`, `ServiceCard`'s three colour maps, `ServiceDetailView.statusPill`, `DashboardView.stats`, `AppSidebar` counters, both charts |
| New notification channel | see §5 |
| New page | route + `adminOnly` + `AppSidebar.vue` nav + API-side `auth, admin` |
| Change card layout/behaviour | `dashboard/ServiceCard.vue` (highest-churn file) |

## 13. Testing

Backend uses stdlib `testing`, table-driven, with `httptest` for HTTP senders and
a throwaway PostgreSQL database per test (`db.OpenTest`, from `ALERTFLOW_TEST_DATABASE_URL`).
Tests live beside the package. There is **no frontend test framework** — verify
UI changes by running the dev servers (see [DEVELOPMENT.md](DEVELOPMENT.md)).

Worth knowing when testing incidents: `OnTransition` accepts a `nil` dispatcher,
so incident logic is testable without any notification plumbing.
