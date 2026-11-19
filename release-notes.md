## v1.0.38

### Fixed

- **Grok 4.7 (including Extra High Fast 500K Max context) forgot previous turns when answering without tool calls**: reversed Cursor IDE 3.30.14's `turn-runner.ts` (`ineligible_terminal_turn`) and traced live KV blobs: Cursor's agent server emits an empty header-only checkpoint (`[system, <user_info>], turns=0`) at turn start, streams `interactionUpdate.turnEnded`, and flushes the user/assistant message `SetBlob` writes plus the terminal `conversationCheckpointUpdate` (~65ms per round-trip) **after** `turnEnded` before closing the `RunSSE` stream. Exiting immediately on `turnEnded` severed the stream before the terminal checkpoint arrived, leaving `cursor-agent-state.bin` stuck on the `turns=0` bootstrap stub while suppressing `ConversationHistory` on the follow-up turn. `readCursorAccountStream` now drains and acknowledges post-`turnEnded` KV blobs through the terminal checkpoint/trailer, and `cursor_account_memory` refuses to persist or resume from empty `turns=0` bootstrap stubs.

> **Windows 用户注意**：安装时若被 SmartScreen 拦截，点击「更多信息」->「仍要运行」即可。

### 下载哪个文件？（按系统选择）

> 名字里的 x64 / x32 / arm64 表示 CPU 架构，认准自己系统的类型下载即可。

- **Windows 64 位（绝大多数 Windows 电脑）**：下载 `cursor-byok-1.0.38-windows-x64-installer.exe`（安装版，推荐）或 `cursor-byok-1.0.38-windows-x64.zip`（绿色版）
- **Windows ARM64（骁龙/麒麟等 ARM 处理器的 Windows 电脑）**：下载 `cursor-byok-1.0.38-windows-arm64-installer.exe` 或 `cursor-byok-1.0.38-windows-arm64.zip`
- **Windows 32 位（很老的低配电脑才需要）**：下载 `cursor-byok-1.0.38-windows-x32-installer.exe` 或 `cursor-byok-1.0.38-windows-x32.zip`
- **macOS Apple Silicon（M1/M2/M3/M4 芯片）**：下载 `cursor-byok-1.0.38-macos-arm64.dmg` 或 `cursor-byok-1.0.38-macos-arm64.tar.gz`
- **macOS Intel**：下载 `cursor-byok-1.0.38-macos-x64.dmg` 或 `cursor-byok-1.0.38-macos-x64.tar.gz`
- **Linux 64 位**：下载 `cursor-byok-1.0.38-linux-x64.tar.gz`

Windows 系统类型可在「设置 → 系统 → 关于 → 系统类型」查看；macOS 在「关于本机」查看芯片或处理器。
