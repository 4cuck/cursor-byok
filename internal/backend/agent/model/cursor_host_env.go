package modeladapter

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"cursor/gen/agentv1"

	"google.golang.org/protobuf/proto"
)

// CursorHostRequestContext reports this process the way the Cursor client
// reports the machine that will run tools: platform plus release, the real
// shell, and the working directory. A hardcoded Windows shell makes the
// cloud agent emit PowerShell on a Linux host.
func CursorHostRequestContext() *agentv1.RequestContext {
	return cursorAccountRequestContext(StreamRequest{})
}

func cursorAccountRequestContext(req StreamRequest) *agentv1.RequestContext {
	var ctx *agentv1.RequestContext
	if req.CursorRequestContext != nil {
		ctx, _ = proto.Clone(req.CursorRequestContext).(*agentv1.RequestContext)
	}
	if ctx == nil {
		ctx = &agentv1.RequestContext{}
	}
	if ctx.Env == nil {
		ctx.Env = &agentv1.RequestContextEnv{}
	}
	fillCursorAccountEnv(ctx.Env)
	cursorAccountMarkContextComplete(ctx)
	return ctx
}

func fillCursorAccountEnv(env *agentv1.RequestContextEnv) {
	if env == nil {
		return
	}
	host := cursorHostEnv()
	if strings.TrimSpace(env.OsVersion) == "" {
		env.OsVersion = host.GetOsVersion()
	}
	if strings.TrimSpace(env.Shell) == "" {
		if cursorEnvMatchesHost(env) {
			env.Shell = host.GetShell()
		} else {
			env.Shell = cursorShellForOS(env.GetOsVersion())
		}
	}
	if strings.TrimSpace(env.TimeZone) == "" {
		env.TimeZone = host.GetTimeZone()
	}
	// The reported OS belongs to the machine that runs commands. Mixing in
	// this process's paths would point a Debian SSH workspace at a Windows
	// directory and the model would go looking with the wrong shell.
	if !cursorEnvMatchesHost(env) {
		return
	}
	if len(env.WorkspacePaths) == 0 {
		env.WorkspacePaths = append([]string(nil), host.GetWorkspacePaths()...)
	}
	if strings.TrimSpace(env.ProjectFolder) == "" {
		env.ProjectFolder = host.GetProjectFolder()
	}
	if env.ProcessWorkingDirectory == nil && host.ProcessWorkingDirectory != nil {
		env.ProcessWorkingDirectory = proto.String(host.GetProcessWorkingDirectory())
	}
	if env.IsWorkingDirHomeDir == nil && host.IsWorkingDirHomeDir != nil {
		env.IsWorkingDirHomeDir = proto.Bool(host.GetIsWorkingDirHomeDir())
	}
}

func cursorAccountMarkContextComplete(ctx *agentv1.RequestContext) {
	if ctx == nil {
		return
	}
	if ctx.EnvInfoComplete == nil {
		ctx.EnvInfoComplete = proto.Bool(true)
	}
	if ctx.RulesInfoComplete == nil {
		ctx.RulesInfoComplete = proto.Bool(true)
	}
	if ctx.RepositoryInfoComplete == nil {
		ctx.RepositoryInfoComplete = proto.Bool(true)
	}
	if ctx.McpInfoComplete == nil {
		ctx.McpInfoComplete = proto.Bool(true)
	}
	if ctx.AgentSkillsInfoComplete == nil {
		ctx.AgentSkillsInfoComplete = proto.Bool(true)
	}
	if ctx.GitStatusInfoComplete == nil {
		ctx.GitStatusInfoComplete = proto.Bool(true)
	}
	if ctx.GitRepoInfoComplete == nil {
		ctx.GitRepoInfoComplete = proto.Bool(true)
	}
	if ctx.CustomSubagentsInfoComplete == nil {
		ctx.CustomSubagentsInfoComplete = proto.Bool(true)
	}
	if ctx.McpFileSystemInfoComplete == nil {
		ctx.McpFileSystemInfoComplete = proto.Bool(true)
	}
}

func cursorHostEnv() *agentv1.RequestContextEnv {
	cwd, _ := os.Getwd()
	home, _ := os.UserHomeDir()
	env := &agentv1.RequestContextEnv{
		OsVersion: cursorHostOSVersion(),
		Shell:     cursorHostShell(),
		TimeZone:  cursorHostTimeZone(),
	}
	if strings.TrimSpace(cwd) == "" {
		return env
	}
	env.WorkspacePaths = []string{cwd}
	env.ProjectFolder = cwd
	env.ProcessWorkingDirectory = proto.String(cwd)
	if home != "" && cursorSamePath(cwd, home) {
		env.IsWorkingDirHomeDir = proto.Bool(true)
	}
	return env
}

func cursorHostOSVersion() string {
	platform := runtime.GOOS
	if platform == "windows" {
		platform = "win32"
	}
	release := strings.TrimSpace(cursorHostRelease())
	if release == "" {
		return platform
	}
	return platform + " " + release
}

func cursorHostShell() string {
	if runtime.GOOS == "windows" {
		return "powershell"
	}
	if shell := strings.TrimSpace(os.Getenv("SHELL")); shell != "" {
		return shell
	}
	if runtime.GOOS == "darwin" {
		return "/bin/zsh"
	}
	return "/bin/bash"
}

func cursorShellForOS(osVersion string) string {
	reported := strings.ToLower(strings.TrimSpace(osVersion))
	switch {
	case strings.HasPrefix(reported, "linux"):
		if runtime.GOOS == "linux" {
			return cursorHostShell()
		}
		return "/bin/bash"
	case strings.HasPrefix(reported, "darwin"), strings.HasPrefix(reported, "mac"):
		if runtime.GOOS == "darwin" {
			return cursorHostShell()
		}
		return "/bin/zsh"
	case strings.HasPrefix(reported, "win"):
		return "powershell"
	default:
		return cursorHostShell()
	}
}

func cursorEnvMatchesHost(env *agentv1.RequestContextEnv) bool {
	reported := strings.ToLower(strings.TrimSpace(env.GetOsVersion()))
	if reported == "" {
		return true
	}
	switch {
	case strings.HasPrefix(reported, "linux"):
		return runtime.GOOS == "linux"
	case strings.HasPrefix(reported, "darwin"), strings.HasPrefix(reported, "mac"):
		return runtime.GOOS == "darwin"
	case strings.HasPrefix(reported, "win"):
		return runtime.GOOS == "windows"
	default:
		return true
	}
}

func cursorHostTimeZone() string {
	if loc := time.Local; loc != nil {
		if name := loc.String(); name != "" && name != "Local" {
			return name
		}
	}
	if zone, _ := time.Now().Zone(); zone != "" {
		return zone
	}
	return "UTC"
}

func cursorSamePath(left, right string) bool {
	left = strings.TrimRight(filepath.Clean(left), `\/`)
	right = strings.TrimRight(filepath.Clean(right), `\/`)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}
