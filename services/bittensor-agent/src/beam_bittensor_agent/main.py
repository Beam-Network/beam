from __future__ import annotations

import argparse
import asyncio
from pathlib import Path

from .policy import SigningPolicy
from .server import AgentServer
from .wallet import BittensorWalletSigner


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser(description="Beam owner-local Bittensor identity agent")
    result.add_argument("--wallet-name", default="default")
    result.add_argument("--wallet-hotkey", default="default")
    result.add_argument("--wallet-path")
    result.add_argument("--socket", default="/run/beam-worker/bittensor.sock")
    return result


async def run(args: argparse.Namespace) -> None:
    signer = BittensorWalletSigner.load(
        name=args.wallet_name,
        hotkey=args.wallet_hotkey,
        path=args.wallet_path,
    )
    server = AgentServer(Path(args.socket), SigningPolicy(signer))
    await server.start()
    try:
        await server.serve_forever()
    finally:
        await server.close()


def main() -> None:
    args = parser().parse_args()
    try:
        asyncio.run(run(args))
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
