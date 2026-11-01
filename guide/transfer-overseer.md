---
id: transfer-overseer
title: Recovery Timeouts
sidebar_label: Recovery Timeouts
sidebar_position: 4
---

# Recovery Timeouts

BeamCore treats each task offer as participant-owned once it is published. Each batch offer carries its deadline in `assignment_timeout_ms`, based on how long similar batches take. Its clock starts when BeamCore publishes the offer.

BeamCore can take a batch back before that deadline when it has delivered nothing while the transfer's other batches are delivering, or late in the transfer when most of it is delivered.

When a batch reaches its deadline or is taken back, BeamCore cancels it with `task_offer_batch_cancel` and reassigns the unfinished work through the same participant-neutral PRISM allocation path. Every other recovery type uses that same reassignment path immediately.

## Why work stopped

The `reason` on `task_offer_batch_cancel` and the `failureReason` on telemetry attempts use these values:

| Value | Meaning |
| --- | --- |
| `deadline_expired` | The batch reached the deadline in its offer. |
| `progress_timeout` | A room lane stopped making progress. |
| `no_delivery_cut` | The batch had delivered nothing and was taken back before that deadline. |
| `tail_cut` | The batch was taken back before that deadline, late in the transfer. |
| `worker_failed` | The participant failed the work. |
| `storage_failed` | The participant reported a storage failure. |
| `path_failed` | The participant reported that a room path was refused. |
| `endpoint_failed` | A room endpoint failed. |
| `beam_side` | Beam failed. |
| `superseded` | Another attempt covered the work. |
| `transfer_ended` | The transfer ended first. |

## Orchestrator obligations

- Keep the NATS control connection and worker gateway healthy.
- Handle the full delivered task-offer wave.
- Forward offers and results immediately.
- Retain each result until BeamCore returns a terminal `task_offer_result_ack`.

## Worker obligations

- Queue every valid offer and execute it as capacity becomes available.
- Report success and failure through `task_offer_result`.
- Start each offer promptly, keep active tasks moving, and report results before the assignment timeout expires.

Stalled or failed work is reassigned and can reduce future PRISM routing share.

## Related pages

- [How Transfers Work](./transfers)
- [Orchestrators](./orchestrators)
- [Workers](./workers)
- [PRISM Scoring](./prism)
