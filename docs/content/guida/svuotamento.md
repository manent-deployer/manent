# Load balancer draining

An in-place deploy on servers or VMs behind a load balancer interrupts the requests in flight and sends traffic to a node that is restarting. With a **pool** in the environment configuration, manent takes each batch of a [wave rollout](ondate.md) out of the load balancer before updating it and puts it back in service only when the load balancer sees it healthy.

```yaml title="envconfig/shop/prod.yml"
rollout:
  waves:
    - {name: canary, select: "canary=true"}
    - {name: rest, select: "canary!=true"}
  maxUnavailable: "1"
traffic:
  pool:
    provider: haproxy
    member: "{host}"            # server name in the backend
    drain_timeout: 5m           # maximum wait for the remaining connections
    min_healthy: "66%"          # healthy members always kept in service
    on_timeout: proceed         # proceed | fail
    settings:
      dataplane_urls: "https://lb1.internal:5555,https://lb2.internal:5555"
      backend: shop
      credentials_secret: haproxy-dataplane
secrets:
  - {name: haproxy-dataplane, ref: "vault:secret/data/lb#dataplane"}
```

The pool is infrastructure: it lives **only** in the environment configuration. A descriptor with a `traffic` section is rejected. The pool requires a wave rollout: without batches every target would be drained at once, and the deploy fails in the `config` step.

## Sequence of a batch

For each batch, around the steps from `deploy` to `activation`:

1. **`drain@<pass>#<batch>`** reads the pool and checks its capacity: if taking the batch out would leave fewer healthy members in service than `min_healthy` (for example because another node is already out for a fault), draining is postponed. manent waits `poll_interval` and tries again (`drain@…#0.1`, `#0.2`, …) until `drain_timeout`, then the pass fails. This check keeps a deploy from finishing off a service that is already degraded.
2. **`drain-wait@…`** waits for the remaining connections to end, with durable reads and sleeps: a five-minute wait survives a worker crash. When `drain_timeout` expires, with `on_timeout: proceed` the deploy goes on (the restart closes the remaining connections); with `fail` the batch goes back in service untouched and the pass fails.
3. The batch steps: `deploy`, `post-test` and custom steps, with the node out of traffic.
4. **`restore@…`** puts the batch back in service before `activation` (or at the end of the batch) and **`restore-wait@…`** waits until the load balancer sees it healthy, up to `restore_timeout`. A node never goes back in service unless it is healthy.

The rollout status (`manent status`, web interface) reports for each pass the members out of the load balancer (`drained`).

## Errors and rollback

With [rollback](rollback.md) enabled, `drain` and `restore` take part in the compensations in reverse order: a batch already back in service is drained again, then returned to the previous release, then put back in service. Traffic thus goes back only to a healthy release. Without rollback, or with `onFailure: stop`, a drained node stays out and the status says so.

In dry-run the pool is not touched.

## Providers

| Provider | Drain | Empty when | Restore and health |
|---|---|---|---|
| `haproxy` | Data Plane API on every active instance: `admin_state: drain` | current sessions (`scur`) at 0 on every instance | `admin_state: ready`; healthy if `operational_state: up` everywhere |
| `aws-elb` | `DeregisterTargets` from the target group, or `EnterStandby` when an Auto Scaling group is set | target no longer `draining` | `RegisterTargets` or `ExitStandby`; healthy if `healthy` in the target group |
| `health-flag` | the application answers 503 to its own health check: an HTTP call or a flag file over SSH | after `drain_delay` | flag removed; healthy if `health_url` answers 200 |
| `f5` | iControl REST on the LTM pool member: `session: user-disabled` (existing and persistent connections go on) | `serverside.curConns` at 0 | `session: user-enabled`, `state: user-up`; healthy if the monitor sees it `up` |
| `nginx-plus` | NGINX Plus API on every instance: `drain: true` on the upstream server | `active` connections at 0 on every instance | `drain: false`, `down: false`; healthy if `up` for the active health checks |

### haproxy

| Setting | Description |
|---|---|
| `dataplane_urls` | Data Plane API URL of every active instance, comma separated: the state changes on all of them. |
| `backend` | Backend with the servers. |
| `credentials_secret` | Secret `user:password` of the Data Plane API. |
| `api_version` | `v3` (default) or `v2`. |
| `ca_file`, `allow_http` | Dedicated CA; `"true"` to allow http URLs (development only). |

### aws-elb

| Setting | Description |
|---|---|
| `region`, `target_group_arn` | Target group of the members. The member is the target ID: the instance ID (`member: "{label.instance_id}"`) or the IP. |
| `port` | Target port (default: the target's, or the target group's). |
| `auto_scaling_group` | With an Auto Scaling group draining uses standby, otherwise the ASG could replace the deregistered instance. |
| `access_key_id_secret`, `secret_access_key_secret`, `role_arn`, … | Credentials, as for the [AWS plugins](../plugins/ecs.md). |

Minimum permissions: `elasticloadbalancing:DescribeTargetHealth`, `RegisterTargets`, `DeregisterTargets` on the target group; with the Auto Scaling group also `autoscaling:DescribeAutoScalingInstances`, `EnterStandby`, `ExitStandby`.

### f5

| Setting | Description |
|---|---|
| `management_url` | Management URL of the active BIG-IP. |
| `pool`, `partition` | LTM pool and partition (default `Common`). The member is `address:port` (`member: "{host}:{port}"`). |
| `credentials_secret` | Secret `user:password` of a role limited to the pool members. |
| `sync_group` | Device group to synchronise (`config-sync to-group`) after every change. |

### nginx-plus

| Setting | Description |
|---|---|
| `api_urls` | API URL of every instance (e.g. `https://lb1:8080/api`), comma separated. |
| `upstream` | `http` upstream with the servers; the member is the server as in the upstream (`member: "{host}:{port}"`). |
| `api_version` | API version (default `9`). |
| `credentials_secret` | Optional: secret `user:password` if the API is protected with basic auth. |

### health-flag

It works with any load balancer, but it does not know the other members of the pool: the capacity check is not available and the wait is a fixed time.

| Setting | Description |
|---|---|
| `mode` | `http` (default) or `file`. |
| `drain_url` | `http`: URL called with `drain_method` (default `PUT`) to drain and with `restore_method` (default `DELETE`) to restore; `{host}`, `{port}` and `{label.<name>}` are replaced. |
| `token_secret` | `http`: secret with a bearer token. |
| `path` | `file`: flag file created and removed over SSH (`ssh_user`, `ssh_key_secret` or `ssh_password_secret`, `known_hosts`). |
| `health_url` | Health probe after the restore (`http(s)://{host}…`). |
| `drain_delay` | Time the load balancer needs to see the target out of service (default 30s). |

## Reference

| `traffic.pool` | Default | Description |
|---|---|---|
| `provider` | | `haproxy`, `aws-elb`, `health-flag`, `f5`, `nginx-plus`. |
| `member` | `{host}` | Member name of a target: `{host}`, `{port}`, `{label.<name>}`. |
| `drain_timeout` | `5m` | Maximum wait for capacity and remaining connections. |
| `restore_timeout` | `5m` | Maximum wait for the restore to become healthy. |
| `poll_interval` | `5s` | Frequency of the reads during the waits. |
| `min_healthy` | `1` | Healthy members to keep in service: a number or a percentage of the pool members. |
| `on_timeout` | `proceed` | `proceed` or `fail`. |
| `settings` | | Provider settings; secrets are referenced by name. |
