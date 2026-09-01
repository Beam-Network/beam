"""Pure validation and safe logging helpers for worker transfers."""

import asyncio
import re
import time
from typing import Any, Dict, Optional
from urllib.parse import parse_qs, urlsplit, urlunsplit

import httpx

try:
    from .models import WorkerState
except ImportError:  # Direct execution support for worker.py
    from models import WorkerState


RANGE_HEADER_RE = re.compile(r"^bytes=(\d+)-(\d+)$")


def task_label(task_id: Optional[str]) -> str:
    """Short task label for logs."""
    return task_id[:16] if task_id else "unknown"


def redact_url(url: str) -> str:
    """Drop query parameters from capability URLs before logging errors."""
    try:
        parts = urlsplit(url)
    except ValueError:
        return url.split("?", 1)[0]
    return urlunsplit((parts.scheme, parts.netloc, parts.path, "", ""))


def exception_detail(error: Exception) -> str:
    """Return a useful exception string without leaking URL query parameters."""
    if isinstance(error, httpx.HTTPStatusError):
        redacted_url = redact_url(str(error.request.url))
        try:
            body = error.response.text[:500].strip()
        except httpx.ResponseNotRead:
            body = ""
        body_detail = f" body={body!r}" if body else ""
        return (
            f"{type(error).__name__}: HTTP {error.response.status_code} "
            f"for {redacted_url}{body_detail}"
        )
    message = str(error)
    return f"{type(error).__name__}: {message}" if message else f"{type(error).__name__}: {error!r}"


def object_storage_route_context(
    destination_url: str,
    route_metadata: Optional[Dict[str, Any]] = None,
) -> Dict[str, Any]:
    """Return safe multipart route fields for logs without exposing signatures."""
    context: Dict[str, Any] = {}
    if route_metadata:
        allowed = (
            "transfer_id",
            "source_id",
            "destination_id",
            "chunk_index",
            "upload_id",
            "part_number",
            "final_object_key",
            "multipart_group_id",
            "multipart_created_at",
            "urls_expires_at",
        )
        context.update({key: route_metadata[key] for key in allowed if route_metadata.get(key) is not None})

    try:
        parts = urlsplit(destination_url)
        query = parse_qs(parts.query)
    except ValueError:
        return context

    if "upload_id" not in context and query.get("uploadId"):
        context["upload_id"] = query["uploadId"][0]
    if "part_number" not in context and query.get("partNumber"):
        context["part_number"] = query["partNumber"][0]
    if "final_object_key" not in context:
        context["final_object_key"] = parts.path.lstrip("/")
    context["destination_host"] = parts.netloc
    return context


def format_route_context(context: Dict[str, Any]) -> str:
    """Format safe route fields in stable order for grep-friendly logs."""
    if not context:
        return ""
    ordered_keys = (
        "transfer_id",
        "source_id",
        "destination_id",
        "chunk_index",
        "upload_id",
        "part_number",
        "final_object_key",
        "multipart_group_id",
        "multipart_created_at",
        "urls_expires_at",
        "destination_host",
    )
    parts = [f"{key}={context[key]}" for key in ordered_keys if context.get(key) is not None]
    return " " + " ".join(parts) if parts else ""


def http_status_detail(error: Exception) -> str:
    """Return HTTP status context for httpx exceptions when available."""
    if isinstance(error, httpx.HTTPStatusError):
        return f" status={error.response.status_code}"
    status_code = getattr(getattr(error, "response", None), "status_code", None)
    return f" status={status_code}" if status_code else ""


def api_key_headers(state: WorkerState) -> Dict[str, str]:
    """Build BeamCore API key headers when the worker has an issued key."""
    return {"X-Api-Key": state.api_key} if state.api_key else {}


def offer_headers(value: Any) -> Dict[str, str]:
    """Return string-only offer headers."""
    if not isinstance(value, dict):
        return {}
    return {str(key): item for key, item in value.items() if isinstance(item, str)}


def parse_offer_range(headers: Dict[str, str]) -> Optional[tuple[int, int, int]]:
    """Parse the signed source Range header as start, end, length."""
    range_header = headers.get("Range") or headers.get("range")
    if not range_header:
        return None
    match = RANGE_HEADER_RE.fullmatch(range_header.strip())
    if not match:
        raise ValueError(f"invalid source Range header: {range_header!r}")
    start = int(match.group(1))
    end = int(match.group(2))
    if end < start:
        raise ValueError(f"invalid source Range header: {range_header!r}")
    return start, end, end - start + 1


def build_transfer_context(task: dict) -> tuple[Optional[dict], Optional[str]]:
    """Validate and normalize the flat worker task offer."""
    source_url = task.get("source_url")
    dest_url = task.get("dest_url")
    if not isinstance(source_url, str) or not source_url.strip():
        return None, "missing_source_url"
    if not isinstance(dest_url, str) or not dest_url.strip():
        return None, "missing_dest_url"

    try:
        chunk_size = int(task.get("chunk_size"))
    except (TypeError, ValueError):
        return None, "invalid_chunk_size"
    if chunk_size <= 0:
        return None, "invalid_chunk_size"

    source_headers = offer_headers(task.get("source_headers"))
    dest_headers = offer_headers(task.get("dest_headers"))
    try:
        parsed_range = parse_offer_range(source_headers)
    except ValueError as exc:
        return None, str(exc)
    if parsed_range is None:
        return None, "missing_source_range"
    range_start, range_end, range_size = parsed_range
    if range_size != chunk_size:
        return None, f"range_size_mismatch:{range_size}!={chunk_size}"

    return {
        "source_url": source_url.strip(),
        "dest_url": dest_url.strip(),
        "chunk_size": chunk_size,
        "range_start": range_start,
        "range_end": range_end,
        "source_headers": source_headers,
        "dest_headers": dest_headers,
        "signed_url_flow": str(task.get("signed_url_flow") or "").strip(),
        "transfer_id": str(task.get("transfer_id") or task.get("task_id") or ""),
        "etag_required": bool(task.get("etag_required")),
    }, None


def remaining_deadline_seconds(deadline_us: int) -> Optional[float]:
    """Return seconds until task deadline, or None when no deadline is set."""
    if deadline_us <= 0:
        return None
    return (deadline_us - time.time() * 1_000_000) / 1_000_000


def is_retryable(error: Exception) -> bool:
    """Check if an error is retryable."""
    if isinstance(error, (asyncio.TimeoutError, httpx.TimeoutException)):
        return True
    if isinstance(error, httpx.HTTPStatusError):
        return error.response.status_code >= 500
    return False


def is_object_storage_presigned_url(url: str) -> bool:
    """Check if URL is an object-storage pre-signed upload URL."""
    if not url:
        return False
    return (
        "X-Amz-Signature" in url
        or "X-Goog-Signature" in url
        or "r2.cloudflarestorage.com" in url
        or "storage.googleapis.com" in url
    )


def is_canary_destination(url: str) -> bool:
    """Check if URL is a canary/null destination."""
    return bool(url) and url.startswith(("null://", "canary://", "skip://"))


def claim_ws_task(state: WorkerState, task_id: str) -> bool:
    """Claim an offered attempt locally so duplicate deliveries share one execution."""
    if task_id in state.active_ws_task_ids:
        return False
    state.active_ws_task_ids.add(task_id)
    return True
