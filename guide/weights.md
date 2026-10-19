---
sidebar_position: 3
title: Weights
---

# Weights

Beam validator weights are the final UID vector submitted to Bittensor. BeamCore materializes the vector from completed production work by qualified orchestrators in the PRISM evidence window, then validators read it and call `set_weights`.

## Final weight formula

BeamCore first computes a base raw score for every qualified orchestrator with a subnet UID:

```text
raw_weight_i = verified_uploaded_mib_i
```

It then ranks qualified orchestrators by `raw_weight` and places them in five fixed-rank tiers, each with a multiplier per MiB:

| Tier  | Rank band      | Tier multiplier |
| ----- | -------------- | --------------- |
| **A** | Top 30         | **1**           |
| **B** | Next 30        | **0.8**         |
| **C** | Next 30        | **0.2**         |
| **D** | Next 30        | **0.08**        |
| **E** | Remaining UIDs | **0.0075**      |

```text
base_weight_i = raw_weight_i x tier_multiplier_i / SUM(raw_weight x tier_multiplier)
```

Positive weight goes to orchestrators with positive raw scores.

BeamCore applies and normalizes fraud-report bonuses into the base, then applies [sell pressure](#sell-pressure), redistribution and weight penalties before final normalization and uint16 encoding.

## Example

Assume 200 qualified orchestrators have equal positive raw scores. The fixed bands and per-UID shares are:

| Tier | Ranks   | Members | Tier share | Per-UID share |
| ---- | ------- | ------- | ---------- | ------------- |
| A    | 1-30    | 30      | 47.62%     | 1.5873%       |
| B    | 31-60   | 30      | 38.10%     | 1.2698%       |
| C    | 61-90   | 30      | 9.52%      | 0.3175%       |
| D    | 91-120  | 30      | 3.81%      | 0.1270%       |
| E    | 121-200 | 80      | 0.95%      | 0.0119%       |

Each tier's share follows its members' raw scores.

## Sell pressure

Miners choose when and how much to sell. Beam adjusts emission weights based on sale volume and frequency. A hotkey earns sell allowance equal to **80%** of its base emissions. **Unused allowance carries forward until spent.** For a hotkey with settled oversale debt, one sale per seven days within the available allowance keeps the sale-volume and sale-frequency factors at 1.00. Base emissions include normalized report rewards and are measured before sell pressure, redistribution and weight penalties.

A sale is any SN105 alpha leaving your coldkey's stake on this subnet, on any hotkey, by any transaction: unstaking, a transfer to another coldkey, a swap to another subnet, or any other outflow. A move between the same coldkey's SN105 hotkeys preserves its sale count and allowance balance. Everything your coldkey sells or sends away in one block is one sale, whatever its size. Sale amounts are measured as gross outflows.

A sale from a miner hotkey counts for that hotkey. A sale from any other hotkey of your coldkey, or from a miner hotkey set as your auto-stake destination, counts as one sale for each of your miner hotkeys, with the amount split in proportion to their emission.

Each sale consumes the available allowance once. The frequency window spans 50,400 blocks. New base emissions add credit to the accumulated allowance.

Selling more, or more often, lowers your weight:

```text
sell_multiplier      = max(0.60, 1 - 2 x alpha sold above available allowance / 7-day emission)
frequency_multiplier = max(0.10, 1 / (1 + 1.5 x (sales in 7 days - 1)))   1.00 for zero or one sale
multiplier           = max(0.10, sell_multiplier x frequency_multiplier)
```

Each α sold above available allowance costs 2 α of emission, collected over the next seven days at 2 × excess ÷ seven-day base, capped at 40% of emission. Any remaining debt keeps the sell multiplier at 0.60 until repaid. At 0.60, 40% of your emission repays it, so each week of base emissions sold above the threshold keeps the multiplier at 0.60 for about 5 weeks.

| Sales in 7 days | 1 | 2 | 3 | 4 | 5 | 6 | 7 or more |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| `frequency_multiplier` | 1.00 | 0.40 | 0.25 | 0.18 | 0.14 | 0.12 | 0.10 |

For an available allowance equal to 80% of seven-day base:

| Last 7 days | Multiplier |
| --- | ---: |
| One sale, up to 80% | 1.00 |
| One sale, 90% | 0.80 for 7 days |
| One sale, 100% | 0.60 for 7 days |
| One sale, 180% | 0.60 for 5 weeks |
| Two sales, up to 80% in total | 0.40 |
| A sale every day | 0.10 |

### Examples

Starting with base emissions of 100 α per 7 days and available allowance of 80 α:

| Sales | Multiplier | Why |
| --- | --- | --- |
| 80 α every 7 days | 1.00 | Within the allowance; it keeps 20 α a week |
| 90 α once | 0.80 for 7 days | 10 α above the threshold costs 20 α |
| 100 α once | 0.60 for 7 days | 20 α above the threshold costs 40 α |
| 180 α once | 0.60 for 5 weeks | 100 α above the threshold costs 200 α |
| 380 α once, after 15 weeks of selling 80 α (300 α kept) | 0.60 for 15 weeks | 300 α above the threshold costs 600 α: selling savings costs twice their amount |
| 40 α, then 40 α within 7 days | 0.40 until the first sale is 7 days old | Within the allowance, but two sales |
| Two sales within 7 days, the second exceeding its available allowance by 20 α | 0.24 (0.60 × 0.40) until the first sale is 7 days old, then 0.60 until the second is | The second sale creates a 40 α charge |
| All of its emission, every day | 0.10 | 7 sales in 7 days; any amount above available credit also adds debt |

Seven days of base emissions at this rate raise an unspent allowance from 80 α to 160 α; selling 160 α once stays within it.

### Redistribution

The weight removed from a hotkey goes to hotkeys of other coldkeys whose multiplier is 1.00, in proportion to the share of their emission they kept.

The share kept is max(0, 1 − sales ÷ base emissions) over the last 7 days. A small miner and a large miner keeping the same proportion receive equal holding scores. The removed weight is fully redistributed. When most miners sell, a holder can receive many times its own emission.

### New hotkeys and debt

Qualification activates participation in the sell-pressure adjustment and redistribution. From the first emission, the multiplier starts at 0.10 and reaches 1.00 after 7 days. A seven-day gap in emission starts a fresh ramp at the next emission.

The multiplier rises evenly: 0.55 after 3.5 days, 1.00 after 7 days.

Oversale debt stays with your coldkey and passes to its other miner hotkeys as registration and qualification change, including a new registration.

For example, if hotkey A owes 600 α and deregisters, a new hotkey registered by the same coldkey owes the 600 α and starts at 0.10, so it stays at 0.60 or below until the debt is repaid.

### Getting back to 1.00

Each selling factor returns to 1.00 according to its own schedule:

| Reduction | Ends |
| --- | --- |
| New hotkey | 7 days after its first emission |
| More than one sale | When only one sale is left in the last 7 days |
| Oversale charge up to 40% of seven-day base | 7 days after the sale |
| Larger oversale charge, or debt | When the debt is repaid, at 0.60: about 5 weeks per week of base emissions sold above the threshold |

Debt repayment continues at its scheduled rate for sales within the allowance. A hotkey that sold all of its emission every day returns to 0.60 within 7 days of its last sale, then stays there until its debt is repaid, often several weeks.

### Checking your state

`GET https://api.b1m.ai/v1/audit/sell-allowance/{hotkey}` returns your hotkey's multipliers, 7-day sales and debt for the latest published epoch. For an allowance-covered sale, sell at most `transferableAlphaRao` in one sale by `windowEndBlock`.

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

The bonus affects base emission shares. Tier membership follows raw uploads; routing and qualification follow workload profiles. Positive uploads and a positive penalty multiplier are required for positive weight.

Report them on the [bug desk](./bug-reports.md) with the **Exploit** or **Security** category.

## Weight vector availability

An empty PRISM evidence window produces an all-zero current summary; validators receive the latest valid positive epoch summary.

For an empty set of valid historical summaries, BeamCore serves the dust vector:

```text
UID 0 burn share      = 90%
active UID dust share = 10% split across eligible recipients
```

An empty dust-recipient set assigns the full vector to UID 0.

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
	"uint16_weights": [32768, 19660, 13107],
	"formula_version": "tier_multiplier_verified_uploaded_mib_x_fraud_report_reward_x_accumulated_allowance_x_sell_allowance_x_penalty_v9",
	"source": "epoch_summary",
	"reward_evaluated_at": "2026-10-01T00:00:00.000Z",
	"all_weights_zero": false
}
```

When `current_epoch` differs from `epoch`, the published vector comes from the latest valid historical summary.

Expiry-adjusted vectors use `source = "epoch_summary_reward_expiry_adjusted"`; `reward_evaluated_at` gives the evaluation time. Include both in the weight proof.

## Improving weight share

- Qualify for each workload by completing its calibration work reliably.
- Complete enough verified uploaded production MiB inside the PRISM evidence window to rank into a higher emission tier.
- Keep your orchestrator connected and ready so it can receive production assignments.
- Maintain strong PRISM performance so routing gives you more opportunities to complete production work.
- Schedule sales within the accumulated allowance and at least seven days apart.
