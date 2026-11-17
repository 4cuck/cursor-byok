//go:build linux || darwin

package modeladapter

import "golang.org/x/sys/unix"

func cursorHostRelease() string {
	var uname unix.Utsname
	if err := unix.Uname(&uname); err != nil {
		return ""
	}
	return unixCString(uname.Release[:])
}

func unixCString(raw []byte) string {
	for index, value := range raw {
		if value == 0 {
			return string(raw[:index])
		}
	}
	return string(raw)
}
