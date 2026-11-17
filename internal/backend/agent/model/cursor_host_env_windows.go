//go:build windows

package modeladapter

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func cursorHostRelease() string {
	info := windows.RtlGetVersion()
	if info == nil || info.MajorVersion == 0 {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d", info.MajorVersion, info.MinorVersion, info.BuildNumber)
}
