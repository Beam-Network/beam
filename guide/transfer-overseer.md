---
id: transfer-overseer
title: Recovery Timeouts
sidebar_label: Recovery Timeouts
sidebar_position: 4
---

# Recovery Timeouts

BeamCore treats each task offer as participant-owned when it is sent. A physical task-offer batch must keep producing newly claimed valid `task_result` messages without a **5 second** gap.

A valid current result, whether successful or failed, refreshes the batch window. Duplicate, invalid, late, conflicting, or superseded results do not. After a blackout, unfinished work is reassigned through the same participant-neutral PRISM allocation path and the intervention contributes to reliability evidence.

## Orchestrator obligations

- Keep the NATS control connection and worker gateway healthy.
- Handle the full delivered task-offer wave.
- Forward offers and results immediately.
- Retain each result until BeamCore returns a terminal `task_result_ack`.

## Worker obligations

- Queue every valid offer and execute it as capacity becomes available.
- Report success and failure through `task_result`.
- Keep active batches producing valid results within the **5 second** window.

Stalled or failed work is reassigned and can reduce future PRISM routing share.

## Related pages

- [How Transfers Work](./transfers)
- [Orchestrators](./orchestrators)
- [Workers](./workers)
- [PRISM Scoring](./prism)
