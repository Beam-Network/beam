---
sidebar_position: 3
title: Weights
---

# Weights

Beam validator weights are the final UID vector submitted to Bittensor. BeamCore materializes the vector from completed production work by qualified orchestrators in the PRISM evidence window, then validators read it and call `set_weights`.

## Final weight formula

BeamCore first computes a base raw score for every qualified orchestrator with a subnet UID:

```text
base_raw_i = verified_uploaded_mib_i * penalty_multiplier_i
```

It then ranks qualified orchestrators by `base_raw` and splits emissions into five fixed-rank tiers:

| Tier  | Rank band      | Nominal emission bucket |
| ----- | -------------- | ----------------------- |
| **A** | Top 30         | **50%**                 |
| **B** | Next 30        | **35%**                 |
| **C** | Next 30        | **10%**                 |
| **D** | Next 30        | **4%**                  |
| **E** | Remaining UIDs | **1%**                  |

Within each active tier, that tier's bucket is split proportionally by `base_raw`:

```text
tier_weight_i      = base_raw_i / SUM(base_raw in tier_i)
normalized_weight_i = effective_tier_bucket_i x tier_weight_i
uint16_weight_i     = floor(normalized_weight_i x 65535)
```

If Tier B, C, D, or E is empty or has zero total raw score, its bucket rolls up into Tier A. Zero-raw orchestrators never receive positive weight.

## Example

Assume 200 qualified orchestrators have equal positive raw scores. The fixed bands and per-UID shares are:

| Tier | Ranks   | Members | Tier bucket | Per-UID share |
| ---- | ------- | ------- | ----------- | ------------- |
| A    | 1-30    | 30      | 50%         | 1.6667%       |
| B    | 31-60   | 30      | 35%         | 1.1667%       |
| C    | 61-90   | 30      | 10%         | 0.3333%       |
| D    | 91-120  | 30      | 4%          | 0.1333%       |
| E    | 121-200 | 80      | 1%          | 0.0125%       |

When raw scores differ, each tier's bucket is divided proportionally instead.

## Inputs

Weights are computed only for orchestrators in the **qualified** pool.

| Input             | Source                                                   |
| ----------------- | -------------------------------------------------------- |
| `verified_uploaded_mib` | Whole MiB from server-planned chunk sizes for completed production tasks in the PRISM evidence window (1 day by default) |
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
	"weights": [0.5, 0.3, 0.2],
	"uint16_weights": [32767, 19660, 13107],
	"formula_version": "tiered_weight_verified_uploaded_mib_x_penalty_v3",
	"all_weights_zero": false
}
```

When `current_epoch` differs from `epoch`, validators are applying the latest valid historical vector because the current PRISM evidence window has no usable production weights.

## Improving weight share

- Graduate to the qualified pool by completing calibration work reliably.
- Complete enough penalty-adjusted uploaded production MiB inside the PRISM evidence window to rank into a higher emission tier.
- Keep your orchestrator connected and ready so it can receive production assignments.
- Maintain strong PRISM performance so routing gives you more opportunities to complete production work.
