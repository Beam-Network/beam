---
id: http
title: HTTP Connector
sidebar_label: HTTP
sidebar_position: 5
---

# HTTP Connector

HTTP transfers use raw BeamCore transfer configs or Python SDK `SourceConfig` / `DestConfig` models. They are useful when the source and destination are already exposed through HTTPS URLs.

## BeamCore API Shape

```http
POST /transfers/create
Content-Type: application/json
X-Api-Key: b1m_...

{
  "sources": [
    {
      "type": "http",
      "url": "https://downloads.example.com/dataset.bin",
      "headers": {
        "Authorization": "Bearer SOURCE_TOKEN"
      }
    }
  ],
  "destinations": [
    {
      "type": "http",
      "url": "https://uploads.example.com/ingest/dataset.bin",
      "headers": {
        "Authorization": "Bearer DEST_TOKEN"
      }
    }
  ],
  "total_size": 104857600,
  "name": "http-to-http"
}
```

Then start assignment:

```http
POST /transfers/distribute
Content-Type: application/json
X-Api-Key: b1m_...

{
  "transfer_id": "uuid"
}
```

## Python SDK

```python
from beam_network_sdk.models import DestConfig, SourceConfig

source = SourceConfig(
    type="http",
    url="https://downloads.example.com/report.parquet",
    headers={"Authorization": "Bearer SOURCE_TOKEN"},
)

destination = DestConfig(
    type="http",
    url="https://uploads.example.com/ingest/report.parquet",
    headers={"Authorization": "Bearer DEST_TOKEN"},
)
```

## Worker Requirements

Workers receive signed or direct HTTP URLs in `task_offer` messages. They must be able to reach both endpoints from their host network.

For efficient parallel transfers, HTTP sources should support byte ranges:

```text
Accept-Ranges: bytes
```

If a source does not support range requests, large transfers may fall back to less efficient execution depending on the prepared task route.
