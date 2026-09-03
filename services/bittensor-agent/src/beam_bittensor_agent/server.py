from __future__ import annotations

import asyncio
import json
import os
import stat
from pathlib import Path
from typing import Any

from .policy import RequestError, SigningPolicy


MAX_REQUEST_BYTES = 64 * 1024


class AgentServer:
    def __init__(self, socket_path: Path, policy: SigningPolicy):
        self.socket_path = socket_path
        self.policy = policy
        self._server: asyncio.AbstractServer | None = None

    async def start(self) -> None:
        self.socket_path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        if self.socket_path.exists():
            mode = self.socket_path.stat().st_mode
            if not stat.S_ISSOCK(mode):
                raise RuntimeError(f"refusing to replace non-socket path {self.socket_path}")
            self.socket_path.unlink()
        self._server = await asyncio.start_unix_server(
            self._handle_connection,
            path=str(self.socket_path),
            limit=MAX_REQUEST_BYTES,
        )
        os.chmod(self.socket_path, 0o600)

    async def serve_forever(self) -> None:
        if self._server is None:
            raise RuntimeError("Bittensor agent server has not been started")
        async with self._server:
            await self._server.serve_forever()

    async def close(self) -> None:
        if self._server is not None:
            self._server.close()
            await self._server.wait_closed()
        if self.socket_path.exists() and self.socket_path.is_socket():
            self.socket_path.unlink()

    async def _handle_connection(
        self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter
    ) -> None:
        try:
            raw = await reader.readline()
            if not raw or len(raw) >= MAX_REQUEST_BYTES:
                raise RequestError("request is empty or too large")
            request = json.loads(raw)
            response = {"ok": True, "result": self.dispatch(request)}
        except (RequestError, json.JSONDecodeError, TypeError, ValueError) as error:
            response = {"ok": False, "error": str(error)}
        except Exception:
            # Do not serialize wallet, library, or key details to the caller.
            response = {"ok": False, "error": "internal signing error"}
        writer.write(json.dumps(response, separators=(",", ":")).encode("utf-8") + b"\n")
        await writer.drain()
        writer.close()
        await writer.wait_closed()

    def dispatch(self, request: dict[str, Any]) -> Any:
        method = str(request.get("method") or "")
        params = request.get("params") or {}
        if not isinstance(params, dict):
            raise RequestError("params must be an object")
        if method == "get_identity":
            return vars(self.policy.identity())
        if method == "sign_enrollment":
            return self.policy.sign_enrollment(params)
        if method == "bind_node_key":
            return self.policy.bind_node_key(params)
        if method == "sign_payment_evidence":
            return self.policy.sign_payment_evidence(params)
        raise RequestError(f"unsupported method {method!r}")
