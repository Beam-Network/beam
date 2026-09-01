"""Worker identity signing and BeamCore registration."""

import asyncio
from typing import Any, Awaitable, Callable, Dict

import httpx

try:
    from .models import WorkerState
except ImportError:  # Direct execution support for worker.py
    from models import WorkerState


_public_ip: str | None = None


async def get_public_ip() -> str:
    """Get and cache the public IP address using fallback services."""
    global _public_ip
    if _public_ip:
        return _public_ip

    services = (
        "https://api.ipify.org",
        "https://ifconfig.me/ip",
        "https://icanhazip.com",
    )
    async with httpx.AsyncClient(timeout=10.0) as client:
        for url in services:
            try:
                response = await client.get(url)
                if response.status_code == 200:
                    _public_ip = response.text.strip()
                    print(f"[Worker] Detected public IP: {_public_ip}")
                    return _public_ip
            except Exception:
                continue
    raise RuntimeError("Failed to detect public IP from any service")


def sign_message(wallet: Any, message: str) -> str:
    """Sign a message with the wallet's hotkey and return a hex signature."""
    return "0x" + wallet.hotkey.sign(message.encode()).hex()


def _registration_payload(wallet: Any, ip: str, port: int, signature: str) -> dict:
    hotkey = wallet.hotkey.ss58_address
    return {
        "hotkey": hotkey,
        "ip": ip,
        "port": port,
        "claimed_bandwidth_mbps": 100,
        "coldkey": wallet.coldkeypub.ss58_address if wallet.coldkeypub else hotkey,
        "signature": signature,
    }


async def register_worker(
    client: httpx.AsyncClient,
    state: WorkerState,
    public_ip_resolver: Callable[[], Awaitable[str]] = get_public_ip,
    signer: Callable[[Any, str], str] = sign_message,
) -> Dict[str, Any]:
    """Register a signed worker identity with BeamCore, retrying transport failures."""
    wallet = state.wallet
    hotkey = wallet.hotkey.ss58_address
    ip = await public_ip_resolver()
    port = 9000
    try:
        signature = signer(wallet, f"{hotkey}:{ip}:{port}")
        print("[Worker] Signed registration message")
    except Exception as error:
        raise Exception(f"Failed to sign registration: {error}") from error

    payload = _registration_payload(wallet, ip, port, signature)
    for attempt in range(3):
        try:
            timeout = 15.0 + (attempt * 10)
            print(f"[Worker] Registration attempt {attempt + 1}/3, timeout={timeout}s")
            response = await client.post(
                f"{state.api_url}/workers/register",
                json=payload,
                timeout=timeout,
            )
            if response.status_code != 200:
                raise Exception(f"HTTP {response.status_code}: {response.text[:200]}")
            data = response.json()
            if not data.get("success"):
                error = (
                    data.get("error")
                    or data.get("detail")
                    or data.get("message")
                    or f"Registration failed: {data}"
                )
                raise Exception(error)
            return data
        except httpx.TimeoutException as error:
            print(f"[Worker] Timeout on attempt {attempt + 1}")
            if attempt == 2:
                raise Exception(f"Timeout connecting to {state.api_url} after 3 attempts") from error
            await asyncio.sleep(2)
        except httpx.ConnectError as error:
            print(f"[Worker] Connection error on attempt {attempt + 1}")
            if attempt == 2:
                raise Exception(f"Connection error to {state.api_url} after 3 attempts") from error
            await asyncio.sleep(2)

    raise RuntimeError("Worker registration exhausted without a result")
