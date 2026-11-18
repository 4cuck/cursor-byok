## v1.0.35

### Fixed

- **Newly created files collapsed onto one single line without newlines**: file creation diffs previously used URL-escaped `diff-match-patch` output (`%0A`), which Cursor IDE's diff fallback parsed as a single `+` line whenever a new file had no prior content. `Write` and `PatchEdit` now emit standard multi-line unified diffs so every added line and line break renders properly in chat cards and diffs.
- **File writes stripped leading indentation and trailing newlines**: `Write` argument decoding no longer trims whitespace from file contents (`contents`, `content`, `fileText`), preserving leading blank lines, indentation, and trailing newlines, with a UTF-8 `fileBytes` fallback for raw Cursor `WriteArgs`.
- **Long Cursor API key turns disconnected around 1M tokens**: `BidiAppend` now proactively refreshes short-lived exchanged API-key JWTs before expiry, renews and resends on HTTP 401 / sign-in rejection, and retries transient HTTP 400/429/502/503/504 and network errors with jittered backoff instead of dropping the active edit stream.
- **Context usage ballooned to 5M+ tokens and forced premature summarization**: thinking buffers now reset per pass and history projection deduplicates repeated `reasoning_content` copies across tool results, stopping multi-tool turns from multiplying stored reasoning tokens.
- **"Continue" got stuck in an endless "Summarizing chat context" loop**: auxiliary calls (compaction summaries, chat titles, commit messages, supervisor reviews, and image descriptions) now run in isolated Cursor conversations without loading or overwriting the chat's saved `cursor-agent-state.bin` checkpoint; blob writes no longer re-append pre-compaction messages; refused checkpoints automatically clear and retry once with bounded history; and failed or canceled compactions close the summarization indicator cleanly.
- **Long-context Grok and Max-mode variants were rejected upstream**: Cursor account requests now fill every required registry parameter (`effort`, `reasoning`, `reasoning_effort`, `fast`, `context`) from the model spec and enable `max_mode` automatically when a Max context window is selected.

### Improved

- **Multiple context windows and Max windows for Cursor account models**: synced Cursor account models expose all catalog context windows in the Model Editor and IDE picker (including 1M windows on Gemini 4 Hydrogen and Claude/GPT/Muse Max variants, plus 256K and 500K Max on Grok 4.7), and pass selected model parameters through to native sub-agents.
- **Faster release builds**: ordinary branch pushes no longer queue a duplicate 6-OS build matrix ahead of release tags, and verification runs in parallel with artifact builds while still gating release publication.

> **Windows 用户注意**：安装时若被 SmartScreen 拦截，点击「更多信息」->「仍要运行」即可。

### 下载哪个文件？（按系统选择）

> 名字里的 x64 / x32 / arm64 表示 CPU 架构，认准自己系统的类型下载即可。

- **Windows 64 位（绝大多数 Windows 电脑）**：下载 `cursor-byok-1.0.35-windows-x64-installer.exe`（安装版，推荐）或 `cursor-byok-1.0.35-windows-x64.zip`（绿色版）
- **Windows ARM64（骁龙/麒麟等 ARM 处理器的 Windows 电脑）**：下载 `cursor-byok-1.0.35-windows-arm64-installer.exe` 或 `cursor-byok-1.0.35-windows-arm64.zip`
- **Windows 32 位（很老的低配电脑才需要）**：下载 `cursor-byok-1.0.35-windows-x32-installer.exe` 或 `cursor-byok-1.0.35-windows-x32.zip`
- **macOS Apple Silicon（M1/M2/M3/M4 芯片）**：下载 `cursor-byok-1.0.35-macos-arm64.dmg` 或 `cursor-byok-1.0.35-macos-arm64.tar.gz`
- **macOS Intel**：下载 `cursor-byok-1.0.35-macos-x64.dmg` 或 `cursor-byok-1.0.35-macos-x64.tar.gz`
- **Linux 64 位**：下载 `cursor-byok-1.0.35-linux-x64.tar.gz`

Windows 系统类型可在「设置 → 系统 → 关于 → 系统类型」查看；macOS 在「关于本机」查看芯片或处理器。