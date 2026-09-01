"""API-key persistence, signed minting, and HTTP authentication headers."""

import json
import logging
import time
import uuid
from typing import Optional

logger = logging.getLogger(__name__)


def load_cached_key(client) -> None:
    try:
        if client._key_cache_path.exists():
            data = json.loads(client._key_cache_path.read_text())
            if data.get("expires") and time.time() < data["expires"] - 60:
                client._api_key = data["key"]
                client._api_key_expires = data["expires"]
                client._api_key_source = "cache"
                logger.info(
                    "Loaded cached API key from disk for %s",
                    client.orchestrator_hotkey[:16],
                )
    except Exception:
        pass


def persist_key(client) -> None:
    try:
        client._key_cache_path.write_text(
            json.dumps(
                {"key": client._api_key, "expires": client._api_key_expires}
            )
        )
    except Exception:
        pass


def clear_cached_key(client) -> None:
    client._api_key = None
    client._api_key_expires = None
    client._api_key_source = None
    try:
        client._key_cache_path.unlink(missing_ok=True)
    except Exception:
        pass


async def ensure_api_key(client, environ) -> Optional[str]:
    if (
        client._api_key
        and client._api_key_expires
        and time.time() < client._api_key_expires - 60
    ):
        if not client._api_key_source:
            client._api_key_source = "cache"
        return client._api_key

    env_api_key = environ.get("BEAMCORE_ORCHESTRATOR_API_KEY")
    if env_api_key and env_api_key.startswith(("b1m_", "bck_")):
        client._api_key = env_api_key
        client._api_key_expires = time.time() + 86400 * 365
        client._api_key_source = "env"
        logger.info(
            "Using BEAMCORE_ORCHESTRATOR_API_KEY from environment for %s...",
            client.orchestrator_hotkey[:16],
        )
        return client._api_key

    if not client.signer:
        logger.error(
            "Cannot get API key: no signer configured and "
            "BEAMCORE_ORCHESTRATOR_API_KEY not set"
        )
        return None
    http_client = await client._get_client()
    try:
        challenge_resp = await http_client.post(
            f"{client.base_url}/auth/challenge",
            json={"hotkey": client.orchestrator_hotkey, "role": "orchestrator"},
        )
        if challenge_resp.status_code == 429:
            retry_after = challenge_resp.headers.get("Retry-After", "300")
            logger.error(
                "Too many auth challenge requests; retry after %s seconds",
                retry_after,
            )
            return None
        if challenge_resp.status_code != 200:
            logger.error("Failed to get auth challenge: %s", challenge_resp.status_code)
            return None
        challenge_data = challenge_resp.json()
        try:
            signature_bytes = client.signer.sign(
                challenge_data["message"].encode("utf-8")
            )
            signature = "0x" + (
                signature_bytes.hex()
                if isinstance(signature_bytes, bytes)
                else str(signature_bytes)
            )
        except Exception as exc:
            logger.error("Failed to sign challenge: %s", exc)
            return None
        verify_resp = await http_client.post(
            f"{client.base_url}/auth/verify",
            json={
                "challenge_id": challenge_data["challenge_id"],
                "hotkey": client.orchestrator_hotkey,
                "signature": signature,
                "role": "orchestrator",
                "key_name": "Orchestrator NATS Control Key",
            },
        )
        if verify_resp.status_code != 200:
            logger.error(
                "Failed to verify signature: %s - %s",
                verify_resp.status_code,
                verify_resp.text,
            )
            return None
        verify_data = verify_resp.json()
        if not verify_data.get("success") or not verify_data.get("api_key"):
            logger.error(
                "Auth verify failed: %s",
                verify_data.get("message", "Unknown error"),
            )
            return None
        client._api_key = verify_data["api_key"]
        client._api_key_expires = time.time() + 86400
        client._api_key_source = "signed"
        client._persist_key()
        logger.info(
            "Obtained API key for orchestrator %s...",
            client.orchestrator_hotkey[:16],
        )
        return client._api_key
    except Exception as exc:
        logger.error("Failed to get API key: %s", exc)
        return None


def auth_headers(client) -> dict:
    timestamp = str(int(time.time()))
    nonce = uuid.uuid4().hex[:8]
    action = "request"
    message = (
        f"orchestrator_auth:{client.orchestrator_hotkey}:"
        f"{timestamp}:{action}:{nonce}"
    )
    if client.signer:
        try:
            signature_bytes = client.signer.sign(message.encode("utf-8"))
            signature = (
                signature_bytes.hex()
                if isinstance(signature_bytes, bytes)
                else str(signature_bytes)
            )
        except Exception as exc:
            logger.warning("Failed to sign auth message: %s", exc)
            signature = "unsigned"
    else:
        signature = "unsigned"
    headers = {
        "X-Hotkey": client.orchestrator_hotkey,
        "X-Orchestrator-Hotkey": client.orchestrator_hotkey,
        "X-Orchestrator-Uid": str(client.orchestrator_uid),
        "X-Orchestrator-Timestamp": timestamp,
        "X-Orchestrator-Nonce": nonce,
        "X-Orchestrator-Signature": signature,
        "X-Orchestrator-Action": action,
    }
    if client._api_key:
        headers["X-Api-Key"] = client._api_key
    return headers


async def get_client(client, httpx_module):
    if client._client is None:
        client._client = httpx_module.AsyncClient(
            timeout=client.timeout,
            event_hooks={"request": [client._inject_auth_headers]},
        )
    return client._client


async def inject_auth_headers(client, request) -> None:
    if "/auth/challenge" not in str(request.url) and "/auth/verify" not in str(
        request.url
    ):
        if not client._api_key:
            await client._ensure_api_key()
    for key, value in client._auth_headers().items():
        request.headers[key] = value
