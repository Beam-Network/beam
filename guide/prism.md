---
sidebar_position: 2
title: PRISM Scoring
---

# PRISM Scoring

**PRISM** (Performance and Reliability Incentive for Subnet Mining) is Beam's orchestrator scoring system. Your **PRISM final score** shapes how much production traffic you receive. Validator emission weights are based on completed qualified production tasks.

## Final score

```text
throughput_score      = fleet_normalize(decayed_assignment_verified_mbps)
reliability_score     = fleet_normalize(raw_reliability)

performance_score     = 0.40 x throughput_score + 0.60 x reliability_score

penalty_multiplier    = decayed_penalty_pressure(fraud, sybil events)

current_ready_gate    = (ready AND connected) ? 1 : 0
active_time_ratio     = active_seconds_in_lookback / lookback_seconds
readiness_multiplier  = current_ready_gate x active_time_ratio

prism_final_score     = performance_score x readiness_multiplier x penalty_multiplier
```

Scores and multipliers are clamped to `[0, 1]`.

## Performance

Beam compares orchestrators in the same pool cohort and maps each value linearly from fleet min to max into the compressed band **`[0.2, 1]`**: the lowest positive throughput in the cohort scores `0.2`, the highest scores `1`, with linear scaling between. Orchestrators with no positive throughput evidence score `0`.

**Throughput** uses provider-verified transfer bandwidth samples. BeamCore keeps per-batch verified bandwidth visible for observability, then combines eligible batches into one task-count-weighted sample per transfer:

```text
transfer_mbps = SUM(batch_mbps x batch_task_count) / SUM(batch_task_count)
```

`first assignment` batches always count toward throughput. Later batches need to be above BeamCore's configured follow-up minimum task count for verified BW (10) to count, while their task outcomes still count for reliability and task totals.

**Throughput** uses recent verified transfer bandwidth with a **1-hour half-life**. A bandwidth sample around 1 hour old contributes about `0.5`; a sample around 2 hours old contributes about `0.25`.

**Reliability** uses time-decayed task outcomes - completions, failures, and reassignment-style failures - over the same window with a **24-hour half-life**. A reliability sample around 24 hours old contributes about `0.5`; a sample around 48 hours old contributes about `0.25`. Raw reliability blends success rate and reassignment rate, then fleet-normalizes on the same compressed min-to-max range before it enters the performance blend.

**Performance** combines throughput and reliability with **40% throughput / 60% reliability** weighting.

## Confidence and pools

Beam routes each orchestrator through one of two pools:

| Pool           | Traffic                           |
| -------------- | --------------------------------- |
| **Qualifying** | Calibration transfers (test mode) |
| **Qualified**  | Production client transfers       |

You begin in **qualifying**. Verified calibration work builds your **confidence score**. At **0.9** confidence, Beam graduates you to **qualified** for production routing and validator weights. Qualified membership persists for routing and scoring on production work.

```text
verified_task_ratio = min(1, verified_task_count / target_verified_tasks)
age_ratio           = min(1, age_days / 7)
maturity_factor     = 0.8 + 0.2 x age_ratio

confidence_score    = verified_task_ratio x success_rate x maturity_factor
```

The verified-task target is about **120** distinct tasks in the evidence window. Confidence also reflects your recent success rate and account age (full maturity around **7 days**).

Right after graduation, routing share uses a mid-tier weight until your first production transfer; it then follows your live PRISM score from production evidence.

Qualifying and qualified orchestrators are scored against peers in the same pool.

## Qualified seasonal reset

Qualified PRISM runs in weekly seasons. A new season starts on Sunday at **00:00 UTC**.

At the start of a season, every qualified orchestrator begins from a neutral routing score of **0.5** until Beam sees fresh qualified PRISM evidence for that season. Readiness still applies, so disconnected or not-ready orchestrators do not receive traffic from the neutral score.

Your first qualified evidence in the season clears the reset status on the next score refresh.

## Readiness

Readiness has two parts:

1. **Current gate** - you must be both **ready** on BeamCore and connected to BeamCore over NATS. If either is false, the folded readiness multiplier is `0`.
2. **Active-time ratio** - BeamCore records readiness and connection transitions, then measures the percentage of the PRISM evidence window where `ready && connected` was true.

The dashboard and APIs show a simple **status**:

| Status        | Meaning                           |
| ------------- | --------------------------------- |
| **Active**    | Connected and ready               |
| **Not ready** | Connected, ready pending          |
| **Inactive**  | Awaiting control-plane connection |

## Penalties

Penalty events in the evidence window shape your penalty multiplier:

| Kind                 | Typical source                                     |
| -------------------- | -------------------------------------------------- |
| **Fraud**            | Fraud penalty record                               |
| **Sybil**            | Sybil violation tied to your hotkey                |

Each event contributes:

```text
pressure += coefficient[kind] x 0.5^(age_hours / 48)
penalty_multiplier = clamp(1 - pressure, 0.2, 1.0)
```

Default coefficient per kind is **0.05**.

The evidence window for tasks, bandwidth samples, readiness active-time, and penalties is **7 days** by default. Reliability samples use a **24-hour** half-life, penalty events use a **48-hour** half-life, and bandwidth samples use a **1-hour** half-life.

Guardrail reassignments feed **reliability** samples.

## Reading your score

Open the [Beam Dashboard](https://data.b1m.ai/weights) for live pool, confidence, and PRISM final score. Use the **Selected Orchestrator** panel for the full breakdown.

For programmatic access:

`GET /orchestrators/prism-scores/:orch_uid`

## Improving your score

- Maintain a stable NATS control connection and set **ready** when your worker pool can receive work.
- Keep your orchestrator active; intermittent downtime reduces the readiness multiplier.
- Complete transfers reliably with steady task completion.
- Complete assigned chunks promptly so provider-verified transfer samples reflect sustained throughput.
- Avoid fraud and sybil penalties that reduce the PRISM final score.
