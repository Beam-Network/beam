---
sidebar_position: 3
title: Weights
---

# Weights

Beam validator weights are the final UID vector submitted to Bittensor. BeamCore materializes the vector from completed production work by qualified orchestrators in the PRISM evidence window, then validators read it and call `set_weights`.

## Final weight formula

BeamCore first computes a base raw score for every qualified orchestrator with a subnet UID:

```text
base_raw_i = task_done_count_i * penalty_multiplier_i
```

It then ranks qualified orchestrators by `base_raw` and splits emissions into three tiers:

| Tier  | Rank band               | Nominal emission bucket |
| ----- | ----------------------- | ----------------------- |
| **A** | Top 10%                 | **80%**                 |
| **B** | Next 30%                | **15%**                 |
| **C** | Remaining orchestrators | **5%**                  |

Within each active tier, that tier's bucket is split proportionally by `base_raw`:

```text
tier_weight_i      = base_raw_i / SUM(base_raw in tier_i)
normalized_weight_i = effective_tier_bucket_i x tier_weight_i
uint16_weight_i     = floor(normalized_weight_i x 65535)
```

If Tier B or Tier C is empty or has zero total raw score, its bucket rolls up into Tier A. Zero-raw orchestrators never receive positive weight.

## Example

Assume ten qualified orchestrators have positive raw score. Tier A contains one orchestrator, Tier B contains 3, and Tier C contains 6.

| UID   | `base_raw`   | Tier | Effective tier bucket | Final weight             |
| ----- | ------------ | ---- | --------------------- | ------------------------ |
| 12    | 50           | A    | 80%                   | 80.00%                   |
| 47    | 30           | B    | 15%                   | 7.50%                    |
| 52    | 20           | B    | 15%                   | 5.00%                    |
| 61    | 10           | B    | 15%                   | 2.50%                    |
| 70-75 | split by raw | C    | 5%                    | proportional share of 5% |

## Inputs

Weights are computed only for orchestrators in the **qualified** pool.

| Input             | Source                                                   |
| ----------------- | -------------------------------------------------------- |
| `task_done_count` | Distinct completed production tasks in the PRISM evidence window (7 days by default) |
| `penalty_multiplier` | Qualified PRISM penalty multiplier from configured penalty pressure |
| UID and hotkey    | Current orchestrator and metagraph state                 |

## No-transfer behavior

If the PRISM evidence window has no completed production tasks, BeamCore marks the current summary as all-zero and serves the last valid nonzero epoch summary to validators.

If no valid historical summary exists, BeamCore falls back to the dust vector:

```text
UID 0 burn share      = 90%
active UID dust share = 10% split across eligible recipients
```

If there are no dust recipients, BeamCore returns a UID 0 burn-only vector.

## Validator consumption

Validators fetch the materialized vector:

```text
GET /Validator/epoch-summary/latest-epoch
```

The response includes matching `uids` and `weights` arrays:

```json
{
	"epoch": 17925,
	"current_epoch": 17926,
	"uids": [12, 47, 52],
	"weights": [0.8, 0.075, 0.05],
	"uint16_weights": [52428, 4915, 3276],
	"formula_version": "tiered_weight_task_done_x_penalty_based",
	"all_weights_zero": false
}
```

When `current_epoch` differs from `epoch`, validators are applying the latest valid historical vector because the current PRISM evidence window has no usable production weights.

## Improving weight share

- Graduate to the qualified pool by completing calibration work reliably.
- Complete enough penalty-adjusted production work inside the PRISM evidence window to rank into a higher emission tier.
- Keep your orchestrator connected and ready so it can receive production assignments.
- Maintain strong PRISM performance so routing gives you more opportunities to complete production work.
