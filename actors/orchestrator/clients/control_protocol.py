"""NATS control-plane URL, subject, TLS, and MsgPack contracts."""

import ssl
from typing import Any, Optional

import msgpack


def validate_nats_url(value: str) -> str:
    clean = value.strip().rstrip("/")
    if clean.startswith(("http://", "https://", "ws://", "wss://")):
        raise ValueError(
            "ORCH_GATEWAY_URL must be a NATS endpoint (nats:// or tls://), "
            "not HTTP/WebSocket"
        )
    if not clean.startswith(("nats://", "tls://")):
        raise ValueError("ORCH_GATEWAY_URL must start with nats:// or tls://")
    return clean


def tls_context_for_url(
    url: str,
    tls_server_name: str,
) -> tuple[Optional[ssl.SSLContext], Optional[str]]:
    if not url.startswith("tls://"):
        return None, None
    return ssl.create_default_context(), tls_server_name or None


def tls_handshake_first_for_url(url: str) -> bool:
    return url.startswith("tls://")


def subject_hotkey(hotkey: str) -> str:
    return hotkey.lower()


def subject(
    direction: str,
    hotkey: str,
    message_type: str,
    *,
    prefix: str,
    environment: str,
) -> str:
    return (
        f"{prefix}.{environment}.{direction}."
        f"{subject_hotkey(hotkey)}.{message_type}"
    )


def pack(envelope: dict[str, Any]) -> bytes:
    return msgpack.packb(envelope, use_bin_type=True)


def unpack(data: bytes) -> dict[str, Any]:
    value = msgpack.unpackb(data, raw=False)
    if not isinstance(value, dict):
        raise ValueError("NATS control envelope must be a map")
    return value
