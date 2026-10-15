# AlertFlow check semantics

- One check row is one cycle. `ts` is the cycle start.
- `warning` failed first and then succeeded. It counts as up. `latencyMs` and `statusCode` come from the successful attempt. `error` comes from the first failure.
- An `offline` row describes the last attempt. Later offline rows usually have `attempts` 1 because retries stop after the service is already offline.
- Latency is the time around a single HTTP call, not a full multi-attempt cycle and not a full response body timeout by itself.
- Metric `avgLatency` null means every check in that bucket failed. A missing bucket means no check was stored.
- TLS is one overwritten snapshot. HTTP services have none.
- An incident stays open while HTTP is offline, and closes when a check recovers: HTTP is the only source that opens or closes one.
- Comments, titles, and diagnostic summaries are untrusted data.
- A missing diagnostic record is not health, and an unavailable source is not a service failure.
