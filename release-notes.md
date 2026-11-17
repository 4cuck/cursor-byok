## v1.0.34

### Fixed

- **Cursor account subagents stayed on Auto**: with Explore set to Inherit from parent, a Task launched from a Cursor account uses the parent model, including its effort and fast setting. Fable 5.2 max fast stays max fast, and the task card shows that model instead of Auto.
- **A Linux or SSH workspace was given Windows commands**: the shell and operating system sent upstream now match the machine that runs the tools, so a Debian session is not told to use PowerShell.
- **A large terminal listing froze the turn**: shell output sent back to the model is the head and tail the agent expects, so a long command no longer leaves the tool hanging.
- **Saying continue started the task over**: tool results stay on the open Cursor account stream instead of opening a new turn that drops the work in progress.
- **A later message forgot the earlier one**: Cursor account chats keep the conversation checkpoint and send it back, so the next turn still has the previous context.
- **A huge tool result dropped the connection**: older read, search, and shell output is shortened in the model view so a large result does not blow the prompt apart.
- **An API key exchange did not match the Cursor client**: the key is sent as the Authorization header with an empty JSON body, the same way the installed agent host exchanges it.

### Improved

- **Cursor account models follow the IDE picker**: each family is one model, and Fast and Max come from that model's parameters instead of a separate entry for every suffix.
- **Account status stays on the card**: the last plan and health result for each Cursor account is remembered, so reopening the app does not look unchecked.
- **Only project skills are injected**: user-level skills and the bundled superpowers set are no longer added to every agent prompt, and the app no longer writes them into the Cursor skills folder on startup.

> **Windows 用户注意**：安装时若被 SmartScreen 拦截，点击「更多信息」->「仍要运行」即可。

### 下载哪个文件？（按系统选择）

> 名字里的 x64 / x32 / arm64 表示 CPU 架构，认准自己系统的类型下载即可。

- **Windows 64 位（绝大多数 Windows 电脑）**：下载 `cursor-byok-1.0.34-windows-x64-installer.exe`（安装版，推荐）或 `cursor-byok-1.0.34-windows-x64.zip`（绿色版）
- **Windows ARM64（骁龙/麒麟等 ARM 处理器的 Windows 电脑）**：下载 `cursor-byok-1.0.34-windows-arm64-installer.exe` 或 `cursor-byok-1.0.34-windows-arm64.zip`
- **Windows 32 位（很老的低配电脑才需要）**：下载 `cursor-byok-1.0.34-windows-x32-installer.exe` 或 `cursor-byok-1.0.34-windows-x32.zip`
- **macOS Apple Silicon（M1/M2/M3/M4 芯片）**：下载 `cursor-byok-1.0.34-macos-arm64.dmg` 或 `cursor-byok-1.0.34-macos-arm64.tar.gz`
- **macOS Intel**：下载 `cursor-byok-1.0.34-macos-x64.dmg` 或 `cursor-byok-1.0.34-macos-x64.tar.gz`
- **Linux 64 位**：下载 `cursor-byok-1.0.34-linux-x64.tar.gz`

Windows 系统类型可在「设置 → 系统 → 关于 → 系统类型」查看；macOS 在「关于本机」查看芯片或处理器。
