from __future__ import annotations

from typing import Any


class BittensorWalletSigner:
    def __init__(self, wallet: Any):
        self._wallet = wallet
        # Resolve the hotkey at startup so an encrypted or invalid wallet fails
        # before the owner-local service accepts requests.
        _ = self._wallet.hotkey

    @classmethod
    def load(cls, name: str, hotkey: str, path: str | None = None) -> "BittensorWalletSigner":
        import bittensor as bt

        options: dict[str, Any] = {"name": name, "hotkey": hotkey}
        if path:
            options["path"] = path
        return cls(bt.Wallet(**options))

    @property
    def hotkey_address(self) -> str:
        return str(self._wallet.hotkey.ss58_address)

    @property
    def coldkey_address(self) -> str:
        coldkey = getattr(self._wallet, "coldkeypub", None)
        return str(coldkey.ss58_address) if coldkey else self.hotkey_address

    def sign(self, message: str) -> str:
        signature = self._wallet.hotkey.sign(message.encode("utf-8"))
        return "0x" + signature.hex()
