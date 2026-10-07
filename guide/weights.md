---
sidebar_position: 3
title: Weights
---

# Weights

Beam validator weights are the final UID vector submitted to Bittensor. BeamCore materializes the vector from completed production work by qualified orchestrators in the PRISM evidence window, then validators read it and call `set_weights`.

## Final weight formula

BeamCore first computes a base raw score for every qualified orchestrator with a subnet UID:

```text
fraud_report_reward_i = 1 + min(1.00, sum(active award bonuses_i))
raw_weight_i = verified_uploaded_mib_i * penalty_multiplier_i * fraud_report_reward_i
```

It then ranks qualified orchestrators by `raw_weight` and splits emissions into five fixed-rank tiers:

| Tier  | Rank band      | Nominal emission bucket |
| ----- | -------------- | ----------------------- |
| **A** | Top 30         | **50%**                 |
| **B** | Next 30        | **35%**                 |
| **C** | Next 30        | **10%**                 |
| **D** | Next 30        | **4%**                  |
| **E** | Remaining UIDs | **1%**                  |

Within each active tier, that tier's bucket is split proportionally by `raw_weight`:

```text
tier_weight_i      = raw_weight_i / SUM(raw_weight in tier_i)
normalized_weight_i = effective_tier_bucket_i x tier_weight_i
uint16_weight_i     = floor(normalized_weight_i x 65535)
```

If Tier B, C, D, or E is empty or has zero total raw score, its bucket rolls up into Tier A. Zero-raw orchestrators never receive positive weight.

BeamCore then adjusts each normalized weight for [sell pressure](#sell-pressure) before computing `uint16_weight`.

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

## Sell pressure

Miners choose when and how much to sell. Beam adjusts emission weights based on sale volume and frequency. In any seven-day window, **one sale** totaling up to **80%** of a hotkey's base emissions causes no reduction from these selling factors. Larger or more frequent sales reduce future emission weights. Base emissions are what the hotkey would receive before this adjustment. The unused portion of the 80% threshold does not carry over.

A sale is any SN105 alpha leaving your coldkey's stake on this subnet, on any hotkey, by any transaction: unstaking, a transfer to another coldkey, a swap to another subnet, or any other outflow. Moving stake between hotkeys on this subnet without changing coldkey is not a sale. Every transaction is one sale, whatever its size. Buying or receiving alpha never offsets a sale.

A sale from a miner hotkey counts for that hotkey. A sale from any other hotkey of your coldkey, or from a miner hotkey set as your auto-stake destination, counts as one sale for each of your miner hotkeys, with the amount split in proportion to their emission.

Selling more, or more often, lowers your weight:

```text
sell_multiplier      = max(0.60, 1 - 2 x alpha sold above the 80% threshold / 7-day emission)
frequency_multiplier = max(0.10, 1 / (1 + 1.5 x (sales in 7 days - 1)))   1.00 with no sale
multiplier           = max(0.10, sell_multiplier x frequency_multiplier)
```

Each α sold above the 80% threshold costs 2 α of emission. Up to 100% sold, this is paid within 7 days. Beyond that, the sell multiplier stays at 0.60 until 2 α of emission per α sold above the 80% threshold has been removed.

| Last 7 days | Multiplier |
| --- | ---: |
| One sale, up to 80% | 1.00 |
| One sale, 90% | 0.80 for 7 days |
| One sale, 100% | 0.60 for 7 days |
| One sale, 180% | 0.60 for 5 weeks |
| Two sales, up to 80% in total | 0.40 |
| A sale every day | 0.10 |

The weight removed from a hotkey goes to hotkeys of other coldkeys whose multiplier is 1.00, in proportion to the share of their emission they kept.

The sell-pressure adjustment applies to an orchestrator once it is qualified, from its first emission: the multiplier starts at 0.10 and reaches 1.00 after 7 days, and starts again after 7 days without emission. Orchestrators still qualifying are outside it and receive none of the removed weight.

Alpha still owed for selling above the 80% threshold stays with your coldkey: if the hotkey deregisters or stops qualifying, it passes to your coldkey's other miner hotkeys, including a new registration.

## Inputs

Weights count verified production bytes from each **qualified workload profile**. Standard storage linked to a room publication is counted once. If raw weights tie, an emissions-only score combines workload scores in proportion to that epoch’s verified bytes. Routing continues to use the score for its own workload.

| Input             | Source                                                   |
| ----------------- | -------------------------------------------------------- |
| `verified_uploaded_mib` | Whole MiB independently verified from production work in the 24-hour evidence window, including verified recovery work |
| `penalty_multiplier` | Verified-byte-weighted independent penalty multiplier across qualified workload profiles |
| `fraud_report_reward` | Active fraud-report awards, from ×1.00 to ×2.00 |
| UID and hotkey    | Current orchestrator and metagraph state                 |

## Fraud report bonuses

Administrators grant emission bonuses for distinct, verified fraud, exploit or security findings:

| Severity | Bonus per finding |
| --- | ---: |
| Critical | +30% |
| High | +20% |
| Medium | +10% |
| Low | +5% |

Bonuses add together, capped at **+100% (×2.00)**. Critical + High gives **+50% (×1.50)**.

Each award expires independently **7 days (168 hours)** after application by default.

Awards follow your hotkey on the same subnet through resets and UID changes. Timers continue during inactivity or qualification.

The bonus affects emission ranking, not routing or qualification. Zero uploads or a zero penalty still produce zero weight.

Report them on the [bug desk](./bug-reports.md) with the **Exploit** or **Security** category.

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

The response includes matching `uids` and `weights` arrays. `workload_scoring` records the workload profiles and emissions-only tie-break score used for the vector:

```json
{
	"epoch": 17925,
	"current_epoch": 17926,
	"uids": [12, 47, 52],
	"weights": [0.5, 0.3, 0.2],
	"uint16_weights": [32767, 19660, 13107],
	"formula_version": "tiered_weight_verified_uploaded_mib_x_penalty_x_fraud_report_reward_v4",
	"source": "epoch_summary",
	"reward_evaluated_at": "2026-10-01T00:00:00.000Z",
	"all_weights_zero": false
}
```

When `current_epoch` differs from `epoch`, validators are applying the latest valid historical vector because the current PRISM evidence window has no usable production weights.

Expiry-adjusted vectors use `source = "epoch_summary_reward_expiry_adjusted"`; `reward_evaluated_at` gives the evaluation time. Include both in the weight proof.

## Improving weight share

- Qualify for each workload by completing its calibration work reliably.
- Complete enough penalty-adjusted uploaded production MiB inside the PRISM evidence window to rank into a higher emission tier.
- Keep your orchestrator connected and ready so it can receive production assignments.
- Maintain strong PRISM performance so routing gives you more opportunities to complete production work.
- One sale totaling up to 80% of seven-day base emissions keeps the sale-volume and sale-frequency multipliers at 1.00; larger or more frequent sales reduce them.
