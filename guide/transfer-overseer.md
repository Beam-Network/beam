---
id: transfer-overseer
title: Recovery Timeouts
sidebar_label: Recovery Timeouts
sidebar_position: 4
---

# Recovery Timeouts

BeamCore treats each task offer as participant-owned once its assignment wave has been dispatched. Each assignment wave has a **15 second** timeout that starts after the full dispatch wave settles.

When the timeout fires, unfinished work is reassigned through the same participant-neutral PRISM allocation path. Every other recovery types use that same reassignment path immediately.

## Orchestrator obligations

- Keep the NATS control connection and worker gateway healthy.
- Handle the full delivered task-offer wave.
- Forward offers and results immediately.
- Retain each result until BeamCore returns a terminal `task_result_ack`.

## Worker obligations

- Queue every valid offer and execute it as capacity becomes available.
- Report success and failure through `task_result`.
- Keep active tasks moving and report results before the assignment timeout expires.

Stalled or failed work is reassigned and can reduce future PRISM routing share.

## Related pages

- [How Transfers Work](./transfers)
- [Orchestrators](./orchestrators)
- [Workers](./workers)
- [PRISM Scoring](./prism)
