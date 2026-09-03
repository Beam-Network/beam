from __future__ import annotations

import dataclasses
import re
from datetime import datetime, timezone
from typing import Any, Protocol


class Signer(Protocol):
    @property
    def hotkey_address(self) -> str: ...

    @property
    def coldkey_address(self) -> str: ...

    def sign(self, message: str) -> str: ...


class RequestError(ValueError):
    """A request failed typed signing policy."""


@dataclasses.dataclass(frozen=True)
class Identity:
    hotkey: str
    coldkey: str


SAFE_IDENTIFIER = re.compile(r"^[A-Za-z0-9_.:-]{1,256}$")
HEX_SHA256 = re.compile(r"^[a-fA-F0-9]{64}$")


class SigningPolicy:
    """Build and sign only the canonical Beam messages approved by policy."""

    def __init__(self, signer: Signer):
        self._signer = signer

    def identity(self) -> Identity:
        return Identity(
            hotkey=self._signer.hotkey_address,
            coldkey=self._signer.coldkey_address,
        )

    def sign_enrollment(self, params: dict[str, Any]) -> dict[str, str]:
        worker_id = required_identifier(params, "worker_id")
        node_public_key = required_identifier(params, "node_public_key")
        nonce = required_identifier(params, "nonce")
        message = ":".join(
            ["beam", "v2", "node-enrollment", worker_id, node_public_key, nonce]
        )
        return signed_response(self._signer, message)

    def bind_node_key(self, params: dict[str, Any]) -> dict[str, str]:
        worker_id = required_identifier(params, "worker_id")
        orchestrator_id = required_identifier(params, "orchestrator_id")
        node_public_key = required_identifier(params, "node_public_key")
        nonce = required_identifier(params, "nonce")
        expires_at = required_timestamp(params, "expires_at")
        message = ":".join(
            [
                "beam",
                "v2",
                "node-delegation",
                self._signer.hotkey_address,
                worker_id,
                orchestrator_id,
                node_public_key,
                expires_at,
                nonce,
            ]
        )
        return signed_response(self._signer, message)

    def sign_payment_evidence(self, params: dict[str, Any]) -> dict[str, str]:
        """Preserve the existing BeamCore payment-evidence canonical message."""

        worker_id = required_identifier(params, "worker_id")
        task_id = required_identifier(params, "task_id")
        offer_id = required_identifier(params, "offer_id")
        chunk_hash = str(params.get("chunk_hash") or "")
        if chunk_hash and not HEX_SHA256.fullmatch(chunk_hash):
            raise RequestError("chunk_hash must be an empty value or hexadecimal SHA-256")
        message = ":".join(
            [
                "beam-worker-payment-evidence",
                worker_id,
                task_id,
                offer_id,
                chunk_hash,
            ]
        )
        return signed_response(self._signer, message)


def required_identifier(params: dict[str, Any], name: str) -> str:
    value = str(params.get(name) or "").strip()
    if not SAFE_IDENTIFIER.fullmatch(value):
        raise RequestError(f"{name} must be a non-empty canonical identifier")
    return value


def required_timestamp(params: dict[str, Any], name: str) -> str:
    value = str(params.get(name) or "").strip()
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as error:
        raise RequestError(f"{name} must be an ISO-8601 timestamp") from error
    if parsed.tzinfo is None:
        raise RequestError(f"{name} must include a timezone")
    if parsed <= datetime.now(timezone.utc):
        raise RequestError(f"{name} must be in the future")
    return parsed.astimezone(timezone.utc).isoformat().replace("+00:00", "Z")


def signed_response(signer: Signer, message: str) -> dict[str, str]:
    return {
        "hotkey": signer.hotkey_address,
        "message": message,
        "signature": signer.sign(message),
    }
