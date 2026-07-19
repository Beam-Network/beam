---
id: hippius
title: Hippius
sidebar_label: Hippius
sidebar_position: 6
---

# Hippius Connector

Hippius support is exposed through SDK provider configs. The SDK process uses the Hippius API token to prepare source/destination access; workers receive executable URLs/routes and never receive the token.

## Python SDK

```python
import asyncio

from beam_network_sdk import BeamSDK
from beam_network_sdk.models import HippiusProviderDestination, HippiusProviderSource


async def main() -> None:
    async with BeamSDK(api_key="b1m_...", base_url="https://beamcore.b1m.ai") as beam:
        result = await beam.transfers.prepare_provider_transfer(
            sources=[
                HippiusProviderSource(
                    bucket="source-bucket",
                    key="datasets/file.bin",
                    api_token="YOUR_HIPPIUS_TOKEN",
                )
            ],
            destinations=[
                HippiusProviderDestination(
                    bucket="dest-bucket",
                    key="archive/file.bin",
                    api_token="YOUR_HIPPIUS_TOKEN",
                )
            ],
            distribute=True,
        )
        print(result.transfer_id)


asyncio.run(main())
```

## TypeScript SDK

```typescript
import { BeamClient, HippiusProviderConfig } from "@beam-network/sdk";

const beam = new BeamClient({ apiKey: process.env.BEAM_API_KEY! });

const transfer = await beam.createTransfer({
  sources: [
    HippiusProviderConfig.create({
      bucket: "source-bucket",
      key: "datasets/file.bin",
      api_token: process.env.HIPPIUS_API_TOKEN!,
    }),
  ],
  destinations: [
    HippiusProviderConfig.create({
      bucket: "dest-bucket",
      key: "archive/file.bin",
      api_token: process.env.HIPPIUS_API_TOKEN!,
    }),
  ],
  name: "hippius-transfer",
});

console.log(transfer.transfer_id);
```

## Configuration

| Field | Required | Description |
|---|---|---|
| `bucket` | yes | Hippius bucket name |
| `key` | yes | Object key |
| `api_token` | yes | Hippius API token used only by the SDK process |
| `base_url` | no | Hippius API base URL, default `https://api.hippius.com` |
| `source_id` / `destination_id` | no | Optional Python SDK labels |

## Notes

- The current SDK surface uses `beam_network_sdk` for Python and `@beam-network/sdk` for TypeScript.
- Sink-mode helpers are not documented here because they are not present in the current SDK package.
- Participant workers execute prepared task URLs; they do not call Hippius directly with your token.
