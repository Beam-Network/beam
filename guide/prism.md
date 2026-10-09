---
id: prism
sidebar_position: 2
title: PRISM Scoring
---

# PRISM Scoring

PRISM rewards orchestrators that finish their assignments reliably and quickly. Each kind of work has its own profile, so good standard-transfer performance does not stand in for room-transfer performance.

*Standard transfers and room transfers are rated today. Streaming and messages are coming later; their scores are unavailable.*

## Every wave is a chance to compete

Within each workload and qualification pool, orchestrators are ordered by assigned task count, then their PRISM final score, then their performance points. Exact ties are settled once for that wave. Task count determines opponents.

Each neighboring pair has one duel, starting at the bottom. The middle participants face two neighbors; the top and bottom face one.

![Orch B is highlighted in mint, with a centered connector branching to its two neighboring duels. The frozen order is orch A, orch B, orch C: 3, 3 and 2 assigned tasks; 3/3, 3/3 and 1/2 verified successes; and trusted durations of 12, 8 and 5 seconds. A precedes B through the pre-wave PRISM tie-break. First, B beats C on reliability: B +5, C −5. Then B beats A on speed at equal reliability: B +15, A −15. Wave totals are A −15, B +20 and C −5. Gains are green and losses red; each endpoint faces B once.](../static/img/prism-duels.png)

*This example follows orch B's two duels: defend against C, then climb against A. A and B have the same assigned load; their pre-wave PRISM places A higher. All three eligible orchestrators participate; the point changes add up to zero.*

Reliability is the assignment's success rate. The higher rate wins, compared before rounding:

```text
successRate = verified successful original tasks / accountable original assigned tasks
```

Neutral exclusions are not accountable tasks. For example, 100% beats 80% regardless of speed. **3/3 and 2/2 both equal 100%**, so their duel is decided by duration. For storage-backed work, completion time comes from verified provider metadata:

```text
assignmentDuration = latest verified original-task upload timestamp from provider metadata
                   − Core publication time of first batch in the assignment
```

Agent-only room transfers have no storage-provider upload; their completion evidence comes from verified delivery receipts.

The shorter duration wins. This interval covers all batches in that wave; it is not averaged or divided by task count. Receiving a task-result message does not establish a storage upload's completion time, and participant-reported timestamps do not decide the duel.

Equal success rates and durations draw; two assignments with no successes also draw. No accountable tasks, or missing trusted timing when rates are equal, means the duel is unrated. Draws and unrated duels award zero points.

The winner gains exactly the points the loser loses. For at least two participants, let `n` be the number competing and `F` the eligible pool size, both frozen before the duel:

```text
r = 1                              if F = 2
r = clamp((n − 2) / (F − 2), 0, 1)  otherwise
Defence stake = 1 + 4r              (1 to 5 points)
Upset stake   = 5 + 10r             (5 to 15 points)
```

Beating the neighbor below earns the defence stake; beating the neighbor above earns the upset stake. A participant alone earns no duel points.

## Reading your score

Performance points are the signed total of duel results settled in the last 24 hours. They can be negative. Each workload and pool normalizes its own totals:

```text
All totals equal: performanceScore = 0.5
Otherwise: performanceScore = 0.2 + 0.8 × (points − lowest) / (highest − lowest)
PRISM final score = performanceScore × readinessMultiplier × penaltyMultiplier
```

Readiness reflects a healthy control connection and availability. Independently verified integrity, fraud and Sybil penalties can reduce the final score. Keep your worker pool reliable and your control connection healthy to improve your opportunities.

Recovery work earns no duel points or qualification progress. A rescue does not erase the failed original assignment. Verified recovery work can still earn work credit. An intervention affects the original task's result once; it adds no second performance penalty.

## Qualification

An orchestrator may be qualified for standard transfers while still qualifying for room transfers, or the reverse. The confidence target is **120 verified original tasks for standard transfers** and **40 for room transfers**. For a qualifying profile:

```text
taskRatio = min(verified original-task successes toward qualification / target, 1)
maturity = 0.8 + 0.2 × min(identity age in hours / 24, 1)
confidenceScore = taskRatio × workload success rate over the last 24 hours × maturity
```

Confidence **>= 0.9** qualifies that workload. The pools compete separately; qualifying points do not carry into the qualified pool.

## Verified bandwidth

```text
verifiedBandwidthMbps = verified bytes × 8 / trusted assignment duration in seconds / 1,000,000
```

Missing or invalid duration produces an unavailable metric. Aggregates are duration-weighted mean assignment goodput, not simultaneous network capacity.

## Scores and history through the API

Use a customer organization key with `telemetry:read`:

```bash
curl 'https://api.b1m.ai/v1/telemetry/participants/orchestrator?workload=standard_transfers' \
  -H 'x-api-key: YOUR_CUSTOMER_KEY'

curl 'https://api.b1m.ai/v1/telemetry/participants/orchestrator/ORCHESTRATOR_ID/history?workload=room_transfers' \
  -H 'x-api-key: YOUR_CUSTOMER_KEY'
```

Read `performancePoints24h`, `performanceScore`, `prismFinalScore`, `pool` and `confidenceScore` for each workload. Participant details include all profiles; lists and history support `workload` filters.

Published telemetry snapshots are available for up to 30 days from their observation time and release one subnet epoch late. Compare records using their snapshot versions and timestamps. See the [API reference](./api-reference) for endpoints and snapshot details.
