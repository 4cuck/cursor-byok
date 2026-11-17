package bridge

import (
	"context"
	"fmt"
	"strings"

	"cursor/internal/cursoraccount"
)

type cursorProcessRuntime struct {
	windows *WindowService
}

func newCursorProcessRuntime(windows *WindowService) cursoraccount.CursorRuntime {
	return cursorProcessRuntime{windows: windows}
}

func (runtime cursorProcessRuntime) Running() bool {
	return runtime.windows != nil && runtime.windows.IsCursorRunning()
}

func (runtime cursorProcessRuntime) Stop(context.Context) error {
	return killCursorProcesses()
}

func (runtime cursorProcessRuntime) Start(context.Context) error {
	// ShellExecuteW with an empty path opens Explorer at the process working
	// directory instead of launching Cursor, so the path must be resolved first.
	path := ""
	if runtime.windows != nil {
		path = runtime.windows.DetectCursorPath("")
	}
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("Cursor executable not found; set the path in settings")
	}
	return launchCursorProcess(path, "")
}
