//go:build !windows && !linux && !darwin

package modeladapter

func cursorHostRelease() string {
	return ""
}
