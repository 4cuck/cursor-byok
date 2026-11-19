## v1.0.39

### Fixed

- **Stopped subagents can now be resumed cleanly when continuing a conversation**:
  - Rewrote native Cursor Account `SubagentArgs.ParentConversationId` and `RootParentConversationId` to the real local conversation ID instead of the ephemeral upstream `RunSSE` UUID, fixing `Subagent parent mismatch for resume` in Cursor IDE and restoring parent `Task` card linking.
  - Preserved `SubagentError.AgentId` and child conversation IDs across stopped, aborted, and parent-canceled `Task` calls, recording a `Task` `tool_result` with `agent_id` (`Task(resume: "<agent_id>")`) plus a summary of partial subagent progress before stopping.
  - Preserved backgrounded subagent completion notices (`agent_id` + output) on Cursor Account follow-up turns instead of stripping them as `<system_reminder>` scaffolding.
- **Eliminated `<end_of_image>` stalls/empty replies and restored automatic context summarization on Cursor API-key / Account models**:
  - Stripped `<end_of_image>` / `<start_of_image>` special tokens from Cursor Account streams and saved checkpoints, automatically retrying when a turn emits only `<end_of_image>` and continuing inline tool turns when `<end_of_image>` appears right after a tool result.
  - Restored automatic 80%-budget context summarization (`escalateForcedPreflightCompaction`) when an upstream Cursor checkpoint (`cursor-agent-state.bin`) exceeds the context window budget, preventing 10+ MB blob re-uploads and multi-decade `GetBlob` delays.
  - Fixed Context Usage reporting (`141% Full` inflation and double-counted `CacheReadTokens`) by using upstream checkpoint `TokenDetails` rather than uncompacted local character heuristics.
  - Preserved Turn 1 workspace environment (`WorkspacePaths`, `OsVersion`, `Shell`) across Turn 2+ realtime `RequestContext` updates so follow-up turns no longer fall back to the app install directory.

> **Windows 用户注意**：安装时若被 SmartScreen 拦截，点击「更多信息」->「仍要运行」即可。

### 下载哪个文件？（按系统选择）

> 名字里的 x64 / x32 / arm64 表示 CPU 架构，认准自己系统的类型下载即可。

- **Windows 64 位（绝大多数 Windows 电脑）**：下载 `cursor-byok-1.0.39-windows-x64-installer.exe`（安装版，推荐）或 `cursor-byok-1.0.39-windows-x64.zip`（绿色版）
- **Windows ARM64（骁龙/麒麟等 ARM 处理器的 Windows 电脑）**：下载 `cursor-byok-1.0.39-windows-arm64-installer.exe` 或 `cursor-byok-1.0.39-windows-arm64.zip`
- **Windows 32 位（很老的低配电脑才需要）**：下载 `cursor-byok-1.0.39-windows-x32-installer.exe` 或 `cursor-byok-1.0.39-windows-x32.zip`
- **macOS Apple Silicon（M1/M2/M3/M4 芯片）**：下载 `cursor-byok-1.0.39-macos-arm64.dmg` 或 `cursor-byok-1.0.39-macos-arm64.tar.gz`
- **macOS Intel**：下载 `cursor-byok-1.0.39-macos-x64.dmg` 或 `cursor-byok-1.0.39-macos-x64.tar.gz`
- **Linux 64 位**：下载 `cursor-byok-1.0.39-linux-x64.tar.gz`

Windows 系统类型可在「设置 → 系统 → 关于 → 系统类型」查看；macOS 在「关于本机」查看芯片或处理器。
