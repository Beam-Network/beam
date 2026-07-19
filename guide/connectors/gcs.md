---
id: gcs
title: Google Cloud Storage
sidebar_label: Google Cloud Storage
sidebar_position: 4
---

# Google Cloud Storage Connector

Google Cloud Storage support is exposed in the Python SDK provider models as `GCSProviderSource` and `GCSProviderDestination`. The current TypeScript SDK provider list does not expose a `GCSProviderConfig`; use the Python SDK or raw HTTP transfer configs until TypeScript GCS support is added.

Participant workers do not use GCS credentials directly. They receive signed URLs or prepared task routes from BeamCore.

## Python

```python
from beam_network_sdk.models import GCSProviderDestination, GCSProviderSource

source = GCSProviderSource(
    bucket="gcp-data-lake",
    key="exports/2026/report.parquet",
    project_id="my-gcp-project",
    credentials_path="/path/to/service-account.json",
)

destination = GCSProviderDestination(
    bucket="gcp-archive",
    key="imports/2026/report.parquet",
    project_id="my-gcp-project",
    credentials_path="/path/to/service-account.json",
)
```

You may pass `service_account_json` instead of `credentials_path` when the SDK process receives credentials from a secret manager.

## Service Account Permissions

For a source bucket, grant read access:

```text
roles/storage.objectViewer
```

For a destination bucket, grant write access:

```text
roles/storage.objectCreator
```

Grant permissions at the bucket level for least privilege.
