---
id: bug-reports
title: Reporting Bugs
sidebar_label: Reporting Bugs
---

# Reporting Bugs

Registered orchestrators report BeamCore bugs on the dashboard at **[/bugs](https://data.b1m.ai/bugs)**, not in public Discord channels. Each report is signed with your orchestrator hotkey, gets an id such as `orc_b1m_4x1h07ca0x`, and keeps a status and timeline you can come back to. Only confirmed **Exploit** and **Security** reports are rewarded, with a [fraud report bonus](./weights.md#fraud-report-bonuses).

## Who can report

Your hotkey must be registered on the subnet, have a BeamCore orchestrator record, and carry no active integrity ban. Validators, workers and other wallets cannot report here.

## Steps

1. **Sign in** at `/bugs`: click **Connect wallet**, choose your orchestrator's coldkey (or its hotkey) among the accounts your wallet shares, pick the orchestrator if that coldkey owns several, and sign the one-line challenge (see [Signing](#signing)).
2. **Describe the bug**: your Discord username (`@name`), category, title, what happened, what you expected and the steps to reproduce. Add a transfer id, a code or log snippet and up to three attachments if they help. The team may invite you to a private thread in the Beam Discord server to discuss the report, so you need to be a member; your username is remembered for your next report.
3. **Sign the report.** The dashboard shows a line containing a hash of the whole report, attachments included. Your signature covers exactly what you send; editing anything afterwards asks for a new signature. Sign within ten minutes.

## Signing

Signing uses a browser wallet extension: the **Bittensor wallet**, **Polkadot.js**, **Talisman** or **SubWallet**. Sign with the **coldkey that owns your orchestrator's hotkey**, which is what wallets usually hold, or with the hotkey itself. The wallet must share that account with data.b1m.ai. It first asks you to allow the site, then to sign the line shown on the page; the line names your orchestrator and the key that signed.

## Limits

| Limit | Value |
| --- | --- |
| Open reports per hotkey | 3 |
| Reports per day | 5 per hotkey, 10 per coldkey |
| Attachments | 3 files, 4 MB each, 8 MB in total |
| Attachment types | PNG, JPEG, WebP screenshots; `.txt`, `.log`, `.json`, `.csv` text |
| Title | 10–120 characters |
| Text fields | 8,000 characters each; snippet 20,000 |

Reports containing a mnemonic, a seed, keyfile contents, a private key or a BeamCore API key are refused. If you pasted one by mistake, treat that wallet or key as exposed.

## Statuses

| Status | Meaning |
| --- | --- |
| Submitted | Received, not yet reviewed |
| In review | The team is looking at it |
| Needs info | The team asked you something; see the timeline or the Discord thread |
| Confirmed | Accepted as a bug |
| Fixed | The fix has shipped |
| Closed | Finished: fixed, not a bug, duplicate or won't fix |

Your report page shows the timeline, every note the team addresses to you, the Discord thread once one is opened, and any bonus granted. Only you see your reports: another hotkey gets *not found*.

## Exploits and security issues

Choose the **Exploit** or **Security** category and do not disclose the issue publicly until it is fixed.
