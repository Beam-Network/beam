---
id: index
title: Connectors
sidebar_label: Overview
sidebar_position: 1
---

# Connectors

Connectors are client SDK helpers for preparing transfers from storage providers. They run in the SDK process, keep provider credentials local, create task-scoped source and destination access, and submit the prepared transfer to BeamCore.

Participant workers do not load connector plugins or provider credentials. Workers receive executable `task_offer` messages with source/destination URLs and headers, then move bytes directly between storage endpoints.

## TypeScript SDK

Install:

```bash
npm install @beam-network/sdk
```

Example:

```typescript
import { BeamClient, R2ProviderConfig, S3ProviderConfig } from "@beam-network/sdk";

const beam = new BeamClient({ apiKey: process.env.BEAM_API_KEY! });

const transfer = await beam.createTransfer({
  sources: [
    R2ProviderConfig.create({
      bucket: "source-bucket",
      key: "exports/report.parquet",
      account_id: process.env.R2_ACCOUNT_ID,
      access_key_id: process.env.R2_ACCESS_KEY_ID!,
      secret_access_key: process.env.R2_SECRET_ACCESS_KEY!,
    }),
  ],
  destinations: [
    S3ProviderConfig.create({
      bucket: "destination-bucket",
      key: "imports/report.parquet",
      region: "us-east-1",
      access_key_id: process.env.AWS_ACCESS_KEY_ID!,
      secret_access_key: process.env.AWS_SECRET_ACCESS_KEY!,
    }),
  ],
  name: "r2-to-s3-report",
});

const status = await beam.waitForTransfer(transfer.transfer_id);
console.log(status.status);
```

## Python SDK

Install:

```bash
pip install beam-network-sdk
```

Example:

```python
import asyncio

from beam_network_sdk import BeamSDK
from beam_network_sdk.models import S3ProviderDestination, S3ProviderSource


async def main() -> None:
    async with BeamSDK(api_key="b1m_...", base_url="https://beamcore.b1m.ai") as beam:
        transfer = await beam.transfers.prepare_provider_transfer(
            sources=[
                S3ProviderSource(
                    bucket="source-bucket",
                    key="exports/report.parquet",
                    region="us-east-1",
                    access_key_id="...",
                    secret_access_key="...",
                )
            ],
            destinations=[
                S3ProviderDestination(
                    bucket="dest-bucket",
                    key="imports/report.parquet",
                    region="us-east-1",
                    access_key_id="...",
                    secret_access_key="...",
                )
            ],
            distribute=True,
        )
        print(transfer.transfer_id)


asyncio.run(main())
```

## Supported Provider Models

| Provider | TypeScript | Python |
|---|---|---|
| Amazon S3 | `S3ProviderConfig` | `S3ProviderSource`, `S3ProviderDestination` |
| Cloudflare R2 | `R2ProviderConfig` | `R2ProviderSource`, `R2ProviderDestination` |
| S3-compatible | `S3CompatibleProviderConfig` | `S3ProviderSource` / `Destination` with `endpoint_url` |
| Google Cloud Storage | See SDK support status | `GCSProviderSource`, `GCSProviderDestination` |
| Hippius | `HippiusProviderConfig` | `HippiusProviderSource`, `HippiusProviderDestination` |
| HTTP | Raw `sources[]` / `destinations[]` transfer configs | `SourceConfig`, `DestConfig` |
