# Callbacks

Callbacks notify external systems (CI pipelines, chat, change management systems) of the outcome of each step and of the final outcome of the deploy. They are declared in the descriptor: up to 5 endpoints, which receive events as `POST` with a JSON body.

```yaml
secrets:
  - name: callback-key
    ref: env:MANENT_CALLBACK_KEY

callbacks:
  - url: https://ci.example.com/hooks/manent
    auth:
      type: hmac
      secret: callback-key
  - url: https://chat.example.com/hooks/deploy
    events: [deployment]      # only start and final outcome
```

| Field | Description |
|---|---|
| `url` | Absolute `https` URL (or `http`, if allowed by the configuration). It cannot contain credentials: pass them with `auth`. |
| `events` | Event types to receive: `step`, `deployment`. Default: all. |
| `auth` | Authentication, optional. See below. |

## Events

A deploy produces events in this order:

| # | `event` | `step` | `status` |
|---|---|---|---|
| 1 | `deployment` | | `started` |
| 2 | `step` | `config` | `succeeded` / `failed` |
| … | `step` | each step of the plan | `succeeded` / `failed` / `skipped` (dry-run only) |
| … | `step` | `checkpoint:<step>`, `rollback:<step>` | `succeeded` / `failed`, only with [rollback](rollback.md) enabled |
| last | `deployment` | | `succeeded` / `failed` |

The final event is sent after the working directory is cleaned up. If a step fails, the following steps do not run and therefore generate no events: after the failed step come the events of any compensations (`rollback:<step>`) and then the final event.

```json title="Step event"
{
  "id": "myapp-1.2.3-test:4",
  "event": "step",
  "sequence": 4,
  "deployment_id": "myapp-1.2.3-test",
  "application": "myapp",
  "environment": "test",
  "step": "artifacts",
  "status": "succeeded",
  "output": {"local_path": "/work/myapp-1.2.3-test/myapp-1.2.3.tar.gz", "sha256": "9f86…", "size": "10240"},
  "timestamp": "2026-10-09T10:00:03Z"
}
```

```json title="Final event with an error"
{
  "id": "myapp-1.2.3-test:7",
  "event": "deployment",
  "sequence": 7,
  "deployment_id": "myapp-1.2.3-test",
  "application": "myapp",
  "environment": "test",
  "status": "failed",
  "error": "step \"deploy\": target 192.168.1.10: connection: …",
  "timestamp": "2026-10-09T10:00:41Z"
}
```

| Field | Description |
|---|---|
| `id` | `<deployment_id>:<sequence>`, deduplication key of the event. |
| `sequence` | Progressive number of the event within the deploy. |
| `dry_run` | Present and `true` in [dry-runs](dry-run.md). |
| `requested_by` | Identity that requested the deploy (e.g. `oidc:alice`), if known. |
| `output` | Step output. It never contains secrets. |
| `error` | Error message, if the step or the deploy failed. |
| `rollback` | Only in the final event of a failed deploy whose policy called for rollback: `completed`, `skipped` (nothing to restore) or `failed`. |

## Headers

| Header | Value |
|---|---|
| `Content-Type` | `application/json` |
| `User-Agent` | `manent` |
| `X-Manent-Event` | `step` or `deployment` |
| `X-Manent-Delivery` | The event `id` |
| `X-Manent-Timestamp`, `X-Manent-Signature` | Only with `hmac` authentication |

## Authentication

`auth.secret` is the **name** of a secret in the `secrets` section, never the value: the descriptor is saved in the database, while the secret is read only at send time. The secret must be declared in the `secrets` section of the descriptor itself: the callback URL is chosen by whoever requests the deploy, so the secrets of the environment configuration (SSH keys, cluster tokens) can never be sent to it. A descriptor that names any other secret is rejected.

| `type` | Extra fields | Effect |
|---|---|---|
| `bearer` | | `Authorization: Bearer <secret>` |
| `basic` | `username` | `Authorization: Basic <username:secret>` |
| `header` | `header` | `<header>: <secret>`, e.g. `X-Api-Key` |
| `hmac` | | Body signature, see below |

### Verifying the HMAC signature

With `hmac` the request carries:

```
X-Manent-Timestamp: 1760004003
X-Manent-Signature: sha256=<hex>
```

where `<hex>` is the HMAC-SHA256, computed with the secret, of the string `<timestamp>.<body>`. The receiver recomputes the signature on the received body, compares it in constant time and rejects requests with a timestamp that is too old, to prevent replays.

```python title="Verification example (Python)"
import hashlib, hmac, time

def verify(secret: bytes, headers, body: bytes, tolerance=300) -> bool:
    ts = headers["X-Manent-Timestamp"]
    if abs(time.time() - int(ts)) > tolerance:
        return False
    expected = hmac.new(secret, ts.encode() + b"." + body, hashlib.sha256).hexdigest()
    received = headers["X-Manent-Signature"].removeprefix("sha256=")
    return hmac.compare_digest(expected, received)
```

## Delivery

- **Durability:** every delivery is a DBOS step of the workflow. After a recovery, deliveries already completed are not repeated.
- **Duplicates:** a delivery interrupted by a crash may arrive twice. The receiver deduplicates on the event `id`.
- **Retries:** a non-`2xx` response, a network error or a timeout make the delivery retry up to `callbacks.max_retries` times. Redirects are not followed and count as errors.
- **Errors:** an undelivered callback is logged but **does not fail the deploy**.
- **Timing:** deliveries happen in sequence inside the workflow. A slow endpoint slows down the deploy, by at most `timeout` per attempt.
- **Cancellation:** a deploy cancelled with `cancel` does not send the final event.

## URL limits { #limiti-sugli-url }

Callback URLs come from whoever requests the deploy: without limits, a descriptor could make the worker call services on the internal network. The `callbacks` section of `manent.yml` restricts them:

```yaml
callbacks:
  timeout: 10s
  max_retries: 3
  allow_http: false                 # https only by default
  allowed_hosts:                    # empty = all hosts
    - ci.example.com
    - "*.hooks.example.com"
```

A URL outside the policy is rejected already at enqueue time (`422` from the API) and checked again before every delivery.
