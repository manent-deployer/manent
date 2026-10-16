# Percentage rollouts

With the `traffic` strategy the new version, the **candidate**, is released next to the one in service, the **stable** version, without receiving traffic. Then manent shifts traffic in steps (for example 1, 5, 25, 50, 100%), with an observation time and a health gate after each step. If something goes wrong traffic goes back to the stable version with a weight change, without redeploying.

```yaml title="envconfig/shop/prod.yml (or descriptor)"
rollout:
  strategy: traffic
  traffic:
    steps: [1, 5, 25, 50, 100]
    bake: 10m                        # after every step but the last
    testers: {header: "x-canary: always"}
    scaleDownDelay: 30m              # the stable version stays ready for a quick return
  approvalsBetween: ["25"]           # pause after the 25% step
  onFailure: rollback-all            # default for this strategy
  health:
    metrics:
      - name: errors
        compare: stable              # candidate against stable
        query: 'sum(rate(http_requests_total{app="{application}",track="{track}",code=~"5.."}[5m])) / sum(rate(http_requests_total{app="{application}",track="{track}"}[5m]))'
        maxRatio: 1.5
        samplesQuery: 'sum(increase(http_requests_total{app="{application}",track="{track}"}[5m]))'
        minSamples: 200
```

**Blue/green** is the same mechanism with a single step: `steps: [100]`, usually with testers and an approval before the switch (`approvalsBetween: ["0"]`).

## Sequence

1. Initial steps (`secrets`, `artifacts`, `qa`, …).
2. **`traffic:prepare`** reads, without changing anything, the stable version and the free slot.
3. **`deploy`** releases the candidate in the free slot, **without traffic**; then the steps up to `activation` excluded (`post-test`, custom steps).
4. **`traffic:testers`** (with `testers`) routes only the testers' requests to the candidate.
5. Approval `"0"`, if required: after the tests at weight 0, before shifting traffic.
6. For each step: **`traffic@<percentage>`** sets the weights, then bake and health gate (`gate@traffic@<percentage>#n`) and, if required, an approval.
7. **`traffic:promote`**: the candidate becomes the version in service; then `activation` and the final steps.
8. After `scaleDownDelay` a separate workflow (`manent.traffic-retire`) scales down the replaced version, without holding the environment queue. If another release changed the version in service in the meantime, it touches nothing.

On the **first release** there is no stable version: no testers nor intermediate steps, the candidate receives all the traffic at once.

## Errors and return

If a step, a gate or a pipeline step fails, with `onFailure` `rollback-wave` or `rollback-all` (the default for this strategy) **`traffic:revert`** sends all the traffic back to the stable version and scales down the candidate; if it had already been promoted, the history goes back to the stable version too. Neither the plugin rollback nor `rollback.enabled` are needed. With `onFailure: stop` the weights stay where they are, for a manual intervention.

**Weight drift.** Before each step manent reads the weights again: if they are not the ones it set (an operator or another tool changed them), the rollout stops (`paused-drift`) and waits for a decision with `manent approve` (continue, overwriting the weights) or `manent reject` (stop and go back to the stable version). With `onDrift: ignore` the check is disabled.

## Comparative gates

At 1% an absolute threshold on errors or latency says little. A metric with `compare: stable` is evaluated twice at the same time, for the candidate and for the stable version, replacing `{track}` with their slot (`{version}` is the candidate version):

- `maxRatio`: maximum candidate/stable ratio;
- `maxDelta`: maximum difference candidate − stable;
- `samplesQuery` and `minSamples`: below the minimum of observed requests the check is not green and the step fails at the end of the bake. A gate that sees no traffic cannot say the release is healthy.

The application metrics need the slot label: on Kubernetes it is `manent.io/track` on the pods, to be carried into Prometheus with relabeling.

## Canary analysis

A threshold on a single reading is fragile: a spike of the stable version, or a few errors of the candidate at low traffic, decide the step. With `analysis: mann-whitney` the metric is judged as in Kayenta: at every reading of the gate manent reads the **series** of candidate and stable over the time elapsed in the step (or `lookback`), with points every `step`, and compares them with the two-sided **Mann-Whitney U** test. The metric fails only when the difference is significant (`p < alpha`) **and** goes in the direction that counts as worse beyond the tolerated ratio between the means.

```yaml title="envconfig/shop/prod.yml"
rollout:
  strategy: traffic
  traffic: {steps: [5, 25, 50, 100], bake: 15m}
  health:
    interval: 1m
    analysis: {pass: 95, marginal: 75}   # score thresholds, 0-100
    metrics:
      - name: errors
        compare: stable
        analysis: mann-whitney
        critical: true                   # red on its own, whatever the score
        query: 'sum(rate(http_requests_total{app="{application}",track="{track}",code=~"5.."}[1m])) / sum(rate(http_requests_total{app="{application}",track="{track}"}[1m]))'
      - name: latency-p99
        provider: datadog
        compare: stable
        analysis: mann-whitney
        maxRatio: 1.2                    # up to +20% is tolerated even if significant
        weight: 2
        query: 'p99:trace.http.request.duration{service:{application},track:{track}}'
      - name: throughput
        provider: cloudwatch
        compare: stable
        analysis: mann-whitney
        direction: decrease              # worse when it drops
        minRatio: 0.8
        query: "SELECT SUM(RequestCount) FROM \"AWS/ApplicationELB\" WHERE TargetGroup = 'shop-{track}'"
```

| Field | Default | Meaning |
|---|---|---|
| `analysis` | | `mann-whitney` for the statistical comparison; without it, the thresholds on a single reading above. Requires `compare: stable`. |
| `provider` | `prometheus` | Metrics system: `prometheus`, `datadog`, `newrelic`, `elastic`, `cloudwatch` (configured in [`rollout`](../operazioni/configurazione.md#rollout)). |
| `direction` | `increase` | What counts as worse: `increase`, `decrease` or `either`. |
| `maxRatio`, `minRatio` | `1` | Tolerated ratio between the means (candidate/stable) in the worse direction. |
| `alpha` | `0.05` | Significance level. |
| `step`, `lookback` | `15s`, time in the step | Resolution and window of the series. |
| `minPoints` | `5` | Fewer points on either side: the metric is **inconclusive**. |
| `weight` | `1` | Weight in the score. |
| `critical` | `false` | A failed critical metric makes the gate red regardless of the score. |

**Score.** With `health.analysis` the gate computes, at every reading, the score of the release: the weighted share of the non-inconclusive metrics that passed, from 0 to 100. During the bake the reading is green from `marginal` up (default 75); the last reading, at the end of the bake, must reach `pass` (default 95) and must have no inconclusive metric, because a step that ended without data cannot say the release is healthy. Probes and alerts stay pass/fail. The score is shown in `rollout.stages[].score` and in the web interface; a red gate lists the failed metrics with means, ratio and p-value, for example `metric errors: candidata 0.2761, stabile 0 (medie di 11 e 11 punti): rapporto +Inf, p=9.3e-05 < 0.05`.

The comparison only makes sense while both versions receive traffic: it runs at the intermediate steps; at 100% and on the first release the comparative metrics are not evaluated.

### Queries per system

Every query is written in the language of its system, with the usual placeholders (`{track}`, `{application}`, …):

| `provider` | Language | Notes |
|---|---|---|
| `prometheus` | PromQL | Instant query for a value, `query_range` with `step` for a series; the first series of the result. |
| `datadog` | metric query (`/api/v1/query`) | Datadog chooses the resolution from the window; `.rollup(avg, 15)` sets it. |
| `newrelic` | NRQL via NerdGraph | Without `SINCE`, `UNTIL` and `TIMESERIES`: manent adds them for the window it reads. The metric is the first numeric field. |
| `elastic` | ES\|QL (`/_query`) | Placeholders `{start}`, `{end}` (RFC 3339) and `{step}` (e.g. `15 seconds`); the last numeric column, one row per bucket sorted by time. |
| `cloudwatch` | metric math or Metrics Insights (`SELECT …`) | Read with `GetMetricData`; the period is rounded to a value CloudWatch accepts (1, 5, 10, 30 seconds or multiples of 60). |

## Routers

| Router | Slots | How traffic moves | Testers |
|---|---|---|---|
| [kubernetes](../plugins/kubernetes.md#rilasci-per-percentuale) with Gateway API | objects with the `-a`/`-b` suffix | weights of the `<service>-a` and `<service>-b` `backendRefs` of an `HTTPRoute` | route rule with a header match |
| [kubernetes](../plugins/kubernetes.md#rilasci-per-percentuale) with Istio | suffixed workloads, shared Service | weights of the routes to the `a` and `b` subsets of a `VirtualService` | `http` entry with a header match |
| [lambda](../plugins/lambda.md#rilasci-per-percentuale) | published versions | alias with an additional weight (`AdditionalVersionWeights`) | direct invocation of the candidate version |
| [cloudrun](../plugins/cloudrun.md#rilasci-per-percentuale) | revisions | service traffic split between two revisions | URL of the revision tag (`testers.tag`) |

On Lambda and Cloud Run versions without traffic use no resources: a blue/green does not double the capacity. An [external plugin](../plugins/esterni.md) can act as a router too, declaring the `slots` and `traffic_router` capabilities.

### Environment router and slots on targets

With plugins that update targets in place (`remote-copy`, `container`, `ecs` with two services), slots are **groups of targets**: every target has the `slot: a` or `slot: b` label, the deploy updates only the targets of the free slot and a router of the environment configuration shifts traffic between the two groups.

```yaml title="envconfig/shop/prod.yml"
targets:
  - {host: 10.0.1.10, labels: {slot: a}}
  - {host: 10.0.2.10, labels: {slot: b}}
traffic:
  router:
    provider: aws-alb
    settings:
      region: eu-west-1
      forward_arn: arn:aws:elasticloadbalancing:eu-west-1:123456789012:listener/app/shop/…
      target_group_a: arn:aws:elasticloadbalancing:eu-west-1:123456789012:targetgroup/shop-a/…
      target_group_b: arn:aws:elasticloadbalancing:eu-west-1:123456789012:targetgroup/shop-b/…
      tester_rule_arn: arn:aws:elasticloadbalancing:eu-west-1:123456789012:listener-rule/app/shop/…
```

With the load balancer routers (`haproxy`, `f5`, `nginx-plus`) the two slots are servers of the same backend: a slot's share is split among its servers, and the `member` setting (default `{host}`, also `{host}:{port}` or `{label.<name>}`) gives the server name of a target. The stable slot is the one with the larger weight in the router; the first release goes to slot `a`. Retirement scales nothing down: the replaced slot stays ready for the next release.

| Router | Settings |
|---|---|
| `haproxy` | As the [haproxy pool](svuotamento.md#haproxy); the weights (0-256) of the backend servers change with the configuration API of the Data Plane API on every instance. |
| `f5` | As the [f5 pool](svuotamento.md#f5); the pool must use *ratio (member)* load balancing: manent changes the members' ratio and disables those with weight 0. |
| `nginx-plus` | As the [nginx-plus pool](svuotamento.md#nginx-plus); manent changes the weight of the upstream servers and drains those with weight 0. |
| `aws-alb` | `region`, `forward_arn` (listener or rule with the forward action), `target_group_a`, `target_group_b`, optional `tester_rule_arn` (a rule, for example on a header, that leads testers to the candidate), credentials as for `aws-elb`. Permissions: `elasticloadbalancing:Describe*`, `ModifyListener` or `ModifyRule`. |

For ECS services with a native strategy (blue/green, linear, canary) there is also the [delegated mode](../plugins/ecs.md#strategie-native): ECS shifts the traffic on its own and manent waits for it to finish.

**Living with GitOps.** If the route is also managed by Argo CD or Flux, the two tools fight over the weights: manent only changes the route rules (with a patch conditioned on the `resourceVersion` it read), and `ignoreDifferences` (Argo CD) or the equivalent must be configured on the weights.

## Status

`manent status`, `manent activity` and the web interface show the stages (one per step), the router, the stable and candidate slots, the current share of the candidate, whether testers are routed and whether the candidate was promoted. The `step` events of the [callbacks](callback.md) include `traffic:*` and `traffic@<percentage>`.

## Reference

| `rollout.traffic` | Default | Description |
|---|---|---|
| `steps` | | Increasing percentages, the last one 100 (at most 20 steps). |
| `bake` | `0` | Observation after every step but the last. |
| `testers.header` | | `"name: value"` for routers that route by header. |
| `testers.tag` | | Tag of the candidate revision (Cloud Run). |
| `scaleDownDelay` | `30m` | Wait before scaling down the replaced version. |
| `onDrift` | `pause` | `pause` or `ignore`. |
| `replicas` | | `proportional`: the candidate starts with one replica and grows before every step up to the replicas of the manifests (plugins implementing `plugin.SlotScaler`, such as `kubernetes`). |

With this strategy `approvalsBetween` accepts `"0"`, the percentages of every step but the last and, with waves, the wave names.

## Across several clusters or regions { #ondate }

With `rollout.waves` the percentage rollout proceeds one pass at a time: for example the European cluster first, then the others. Every pass (a cluster or a region, according to `select` and `by` of the [waves](ondate.md)) runs the whole sequence (preparation, candidate, testers, steps, promotion) before the next one, and a wave listed in `approvalsBetween` pauses the rollout before moving to the following wave. Steps carry the pass name (`traffic@25@eu`, `deploy@eu`), and so do stages (`eu/25%`); freezes per region apply as in waves. On error `rollback-wave` returns only the failed pass to the stable version, `rollback-all` also the ones already promoted, in reverse order.

```yaml
rollout:
  strategy: traffic
  waves:
    - {name: eu, select: "region=eu-1"}
    - {name: rest, select: "region!=eu-1", by: region}
  traffic: {steps: [5, 25, 100], bake: 15m}
  approvalsBetween: ["eu"]
```

Waves need a router in the plugin (one pass per cluster or region): an environment router (`traffic.router`) shifts traffic for the whole environment and is not allowed with waves. Passes run on the worker that runs the deploy, also with region routing.
