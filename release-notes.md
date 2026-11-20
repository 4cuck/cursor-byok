## v1.0.51

### Fixed

- **Cursor account file edits now show accurate line diffs (e.g. `-30` instead of `+20`) and no spurious `Read` cards**:
  - Upstream Cursor `EditToolCall` lifecycle updates (`PartialToolCall`, `ToolCallDelta`, `ToolCallStarted`, `ToolCallCompleted`) are now forwarded directly with authoritative `beforeFullFileContent`, `afterFullFileContent`, `diffString`, `linesAdded`, and `linesRemoved`.
  - Low-level internal `ReadArgs` / `WriteArgs` RPCs behind a Cursor-owned edit execute silently on the IDE without creating fake `Read attempted` / `Read` bubbles or synthetic `+N` write cards.
  - Standalone native writes fall back to computing diffs against complete file snapshots observed in the stream, and `EditSuccess` preserves literal leading/trailing whitespace and newlines.
- **Cursor account native web tools, spill writes, and wire correlation**:
  - Native `WebSearch` / `WebFetch` approval and upstream result completion cards are forwarded cleanly with timeout and cancel cleanup.
  - Internal `agent-tools/*.txt` spill writes run silently in all modes without emitting edit cards or polluting history.
  - Tightened exec/interaction wire correlation (including zero-ID execs, out-of-order replies, and `stream_close` on all terminal execs), non-streaming shell output delivery, and prompt/resume scaffolding filtering.
- **Provider streaming and replay stability**:
  - Anthropic streams complete cleanly on `message_stop` without waiting for socket close; OpenAI chat streams fail cleanly if cut off before tool completion; oversized `Grep` replay results compact to actual matches.

## v1.0.50

### Fixed

- **A finished shell no longer leaves the turn on "Planning next moves"**:
  - Cursor account shells now send the captured output, then close the upstream exec, so the model can read the result.
  - Output that the IDE spilled to a file is inlined. The model is not sent that path when the text is already included.
  - A shell that streams output but never exits is recovered and closed, instead of leaving the account stream open.

> **Windows 用户注意**：安装时若被 SmartScreen 拦截，点击「更多信息」->「仍要运行」即可。

### 下载哪个文件？（按系统选择）

> 名字里的 x64 / x32 / arm64 表示 CPU 架构，认准自己系统的类型下载即可。

- **Windows 64 位（绝大多数 Windows 电脑）**：下载 `cursor-byok-1.0.51-windows-x64-installer.exe`（安装版，推荐）或 `cursor-byok-1.0.51-windows-x64.zip`（绿色版）
- **Windows ARM64（骁龙/麒麟等 ARM 处理器的 Windows 电脑）**：下载 `cursor-byok-1.0.51-windows-arm64-installer.exe` 或 `cursor-byok-1.0.51-windows-arm64.zip`
- **Windows 32 位（很老的低配电脑才需要）**：下载 `cursor-byok-1.0.51-windows-x32-installer.exe` 或 `cursor-byok-1.0.51-windows-x32.zip`
- **macOS Apple Silicon（M1/M2/M3/M4 芯片）**：下载 `cursor-byok-1.0.51-macos-arm64.dmg` 或 `cursor-byok-1.0.51-macos-arm64.tar.gz`
- **macOS Intel**：下载 `cursor-byok-1.0.51-macos-x64.dmg` 或 `cursor-byok-1.0.51-macos-x64.tar.gz`
- **Linux 64 位**：下载 `cursor-byok-1.0.51-linux-x64.tar.gz`

Windows 系统类型可在「设置 → 系统 → 关于 → 系统类型」查看；macOS 在「关于本机」查看芯片或处理器。
