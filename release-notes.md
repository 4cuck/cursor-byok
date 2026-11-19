## v1.0.40

### Fixed

- **Preserved full conversation context across Cursor Account model switches, retries, rewinds, and history replays**:
  - Added per-turn pre-turn Cursor checkpoint snapshots (`cursor-agent-turn-states/before-turn-<N>.bin`, bounded to the latest 20 turns) right before each non-prewarm turn starts, preserving the original snapshot across repeated retries of the same turn.
  - Updated rewind/retry handling (`handleRunIntent`) to restore the pre-turn upstream checkpoint (`RestoreCursorAccountStateBeforeTurn`) instead of unconditionally deleting `cursor-agent-state.bin`, so switching models and retrying a failed/aborted turn keeps the full upstream context.
  - Replaced the ignored `UserMessageAction.ConversationHistory` fallback with native `ConversationStateStructure.RootPromptMessagesJson` + SHA-256 KV blob synthesis (`cursorAccountSynthesizeRootPromptMessages`), so replays after rewind, `/summarize` compaction, third-party BYOK model switches, or checkpoint rejection restore full conversation history instead of dropping to a blank 2% context.
  - Prepended a `<user_info>` recovery placeholder (`requestContextCompleteness` all false) and marked all 9 `RequestContext` completeness flags so Cursor's cloud agent server renders a fresh full `<user_info>` while preserving replayed history.
  - Added `sanitizeCursorAccountHistoryMessages` to strip empty assistant placeholders, merge adjacent parallel assistant tool-call entries, drop unanswered tool calls, and convert orphan tool results into user notes so Claude/Grok/Gemini accept replayed tool transcripts cleanly.
  - Preserved `<conversation_summary>` / `[Previous conversation summary]` blocks across scaffolding filtering and context-window bounding (`cursorAccountBoundRootBlobs`), and cleared stale checkpoint snapshots on compaction, overflow compaction, and non-Cursor-account turns (`ClearCursorAccountStateAndSnapshots`).
- **Improved Cursor Account stream stability and checkpoint retention**:
  - Reset the silent timeout timer on `KvServerMessage` (`GetBlob` / `SetBlob`) activity so large history restores no longer trigger false access-denial timeouts.
  - Stopped discarding valid saved checkpoints on transient content safety refusals (`refused to serve this request based on the content`) and `invalid parameters for registry model` errors.

> **Windows 用户注意**：安装时若被 SmartScreen 拦截，点击「更多信息」->「仍要运行」即可。

### 下载哪个文件？（按系统选择）

> 名字里的 x64 / x32 / arm64 表示 CPU 架构，认准自己系统的类型下载即可。

- **Windows 64 位（绝大多数 Windows 电脑）**：下载 `cursor-byok-1.0.40-windows-x64-installer.exe`（安装版，推荐）或 `cursor-byok-1.0.40-windows-x64.zip`（绿色版）
- **Windows ARM64（骁龙/麒麟等 ARM 处理器的 Windows 电脑）**：下载 `cursor-byok-1.0.40-windows-arm64-installer.exe` 或 `cursor-byok-1.0.40-windows-arm64.zip`
- **Windows 32 位（很老的低配电脑才需要）**：下载 `cursor-byok-1.0.40-windows-x32-installer.exe` 或 `cursor-byok-1.0.40-windows-x32.zip`
- **macOS Apple Silicon（M1/M2/M3/M4 芯片）**：下载 `cursor-byok-1.0.40-macos-arm64.dmg` 或 `cursor-byok-1.0.40-macos-arm64.tar.gz`
- **macOS Intel**：下载 `cursor-byok-1.0.40-macos-x64.dmg` 或 `cursor-byok-1.0.40-macos-x64.tar.gz`
- **Linux 64 位**：下载 `cursor-byok-1.0.40-linux-x64.tar.gz`

Windows 系统类型可在「设置 → 系统 → 关于 → 系统类型」查看；macOS 在「关于本机」查看芯片或处理器。
