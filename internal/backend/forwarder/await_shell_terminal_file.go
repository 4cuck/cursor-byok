// await_shell_terminal_file.go parses the terminal output file the Cursor
// client keeps for every shell: a header block that is rewritten in place
// while the command runs, the raw output, and a footer block appended once the
// command ends. The client's own AwaitShell decides completion from this file,
// so it is the source of truth here as well.
package forwarder

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

const terminalFileFence = "\n---\n"

var (
	terminalFooterExitCodePattern = regexp.MustCompile(`(?m)^exit_code:[ \t]*([^\n]*)$`)
	terminalFooterErrorPattern    = regexp.MustCompile(`(?m)^error:[ \t]*([^\n]+)$`)
	terminalFooterElapsedPattern  = regexp.MustCompile(`(?m)^elapsed_ms:[ \t]*(\d+)[ \t]*$`)
	terminalFooterEndedAtPattern  = regexp.MustCompile(`(?m)^ended_at:[ \t]*(\S+)[ \t]*$`)

	// terminalHeaderMarkerPattern matches a line that only the client's header
	// block has. Command output fenced by its own dash rules (YAML documents,
	// markdown front matter) does not carry one.
	terminalHeaderMarkerPattern = regexp.MustCompile(`(?m)^(?:pid:[ \t]*\d+|running_for_ms:[ \t]*\d+|started_at:[ \t]*\S|last_command:|active_command:|last_exit_code:)`)
	// terminalHeaderFieldPattern matches one field line of a shell's header.
	terminalHeaderFieldPattern = regexp.MustCompile(`^(?:pid|cwd|command|title|status|started_at|running_for_ms):`)
)

// terminalHeaderMaxLines is the longest header the client writes for a shell:
// pid, cwd, command, title, status, started_at, running_for_ms.
const terminalHeaderMaxLines = 7

// terminalFileView is one reading of a terminal file. A ranged read that only
// covers the end of a long file has no header, or just the last lines of one;
// everything else still applies.
type terminalFileView struct {
	HasHeader    bool
	Status       string
	PID          *uint32
	Command      string
	CWD          string
	StartedAt    time.Time
	RunningForMS *int64

	// Complete is true once the footer is present: the command has ended.
	Complete    bool
	ExitCode    *int32
	ElapsedMS   *int64
	EndedAt     time.Time
	FooterError string

	// Body is the command output with header and footer removed.
	Body string
}

func parseTerminalFileView(content string) terminalFileView {
	text := content
	if strings.Contains(text, "\r\n") {
		text = strings.ReplaceAll(text, "\r\n", "\n")
	}
	view := terminalFileView{}

	bodyStart := 0
	if header, next, ok := leadingTerminalFileHeader(text); ok {
		view.HasHeader = true
		parseTerminalFileHeader(header, &view)
		bodyStart = next
	} else if fields, next, ok := leadingTerminalFileHeaderRemainder(text); ok {
		parseTerminalFileHeader(fields, &view)
		bodyStart = next
	}
	if bodyStart >= len(text) {
		// Header only: no output yet.
		return view
	}

	bodyEnd := len(text)
	if footerStart, footer, ok := locateTerminalFileFooter(text, bodyStart); ok {
		bodyEnd = footerStart
		view.Complete = true
		parseTerminalFileFooter(footer, &view)
	}
	if bodyStart < bodyEnd {
		view.Body = text[bodyStart:bodyEnd]
	}
	return view
}

// leadingTerminalFileHeader returns the header block at the start of the text
// and the offset where the output begins. A text that merely starts with a
// dash rule is not a header: a ranged read begins wherever the last lines of
// the file begin, and that can be a rule the command itself printed.
func leadingTerminalFileHeader(text string) (string, int, bool) {
	if !strings.HasPrefix(text, "---\n") {
		return "", 0, false
	}
	if closing := strings.Index(text[3:], terminalFileFence); closing > 0 {
		header := text[4 : 3+closing]
		if !terminalHeaderMarkerPattern.MatchString(header) {
			return "", 0, false
		}
		return header, 3 + closing + len(terminalFileFence), true
	}
	// Header only: no output yet and no newline after the closing fence.
	if trimmed := strings.TrimRight(text, " \t\n"); strings.HasSuffix(trimmed, "\n---") && len(trimmed) > 8 {
		header := trimmed[4 : len(trimmed)-4]
		if !terminalHeaderMarkerPattern.MatchString(header) {
			return "", 0, false
		}
		return header, len(text), true
	}
	return "", 0, false
}

// leadingTerminalFileHeaderRemainder recognizes a text that begins inside the
// header: a ranged read of a file a few lines longer than the range starts
// there. What is left of the header is its last fields, always ending in
// running_for_ms, and the closing fence. Those lines are metadata, not output.
func leadingTerminalFileHeaderRemainder(text string) (string, int, bool) {
	offset := 0
	for line := 0; line <= terminalHeaderMaxLines; line++ {
		end := strings.IndexByte(text[offset:], '\n')
		if end < 0 {
			return "", 0, false
		}
		current := text[offset : offset+end]
		if current == "---" {
			if line == 0 {
				return "", 0, false
			}
			fields := text[:offset-1]
			lastField := fields[strings.LastIndexByte(fields, '\n')+1:]
			if !strings.HasPrefix(lastField, "running_for_ms:") {
				return "", 0, false
			}
			return fields, offset + end + 1, true
		}
		if !terminalHeaderFieldPattern.MatchString(current) {
			return "", 0, false
		}
		offset += end + 1
	}
	return "", 0, false
}

// locateTerminalFileFooter finds the trailing footer block. The file must end
// with a fence, the block before it must be opened by another fence at or
// after the end of the header, and the block must look like the client's
// footer: an exit_code (or error) line plus timing metadata. Output that
// merely ends in a dash rule is not a footer.
func locateTerminalFileFooter(text string, bodyStart int) (int, string, bool) {
	trimmed := strings.TrimRight(text, " \t\n")
	if !strings.HasSuffix(trimmed, "\n---") {
		return 0, "", false
	}
	closing := len(trimmed) - len("\n---")
	opener := strings.LastIndex(trimmed[:closing], terminalFileFence)
	if opener < 0 {
		return 0, "", false
	}
	// The header's closing fence and the footer's opening fence may share a
	// newline when the command printed nothing.
	if opener < bodyStart-1 {
		return 0, "", false
	}
	footer := trimmed[opener+len(terminalFileFence) : closing]
	if strings.TrimSpace(footer) == "" {
		return 0, "", false
	}
	hasOutcome := terminalFooterExitCodePattern.MatchString(footer) || terminalFooterErrorPattern.MatchString(footer)
	hasTiming := terminalFooterElapsedPattern.MatchString(footer) || terminalFooterEndedAtPattern.MatchString(footer)
	if !hasOutcome || !hasTiming {
		return 0, "", false
	}
	start := opener
	if start < bodyStart {
		start = bodyStart
	}
	return start, footer, true
}

func parseTerminalFileFooter(footer string, view *terminalFileView) {
	if match := terminalFooterExitCodePattern.FindStringSubmatch(footer); match != nil {
		if parsed, err := strconv.ParseInt(strings.TrimSpace(match[1]), 10, 32); err == nil {
			exitCode := int32(parsed)
			view.ExitCode = &exitCode
		}
	}
	if match := terminalFooterErrorPattern.FindStringSubmatch(footer); match != nil {
		view.FooterError = strings.TrimSpace(match[1])
	}
	if match := terminalFooterElapsedPattern.FindStringSubmatch(footer); match != nil {
		if parsed, err := strconv.ParseInt(match[1], 10, 64); err == nil {
			view.ElapsedMS = &parsed
		}
	}
	if match := terminalFooterEndedAtPattern.FindStringSubmatch(footer); match != nil {
		view.EndedAt = parseRFC3339Time(match[1])
	}
}

func parseTerminalFileHeader(header string, view *terminalFileView) {
	for _, line := range strings.Split(header, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = parseTerminalShellMetadataValue(value)
		switch strings.TrimSpace(key) {
		case "pid":
			if parsed, err := strconv.ParseUint(value, 10, 32); err == nil {
				pid := uint32(parsed)
				view.PID = &pid
			}
		case "cwd":
			view.CWD = value
		case "command":
			view.Command = value
		case "status":
			// The value is padded so the client can rewrite it in place.
			if fields := strings.Fields(value); len(fields) > 0 {
				view.Status = fields[0]
			}
		case "started_at":
			view.StartedAt = parseRFC3339Time(value)
		case "running_for_ms":
			if fields := strings.Fields(value); len(fields) > 0 {
				if parsed, err := strconv.ParseInt(fields[0], 10, 64); err == nil {
					view.RunningForMS = &parsed
				}
			}
		}
	}
}

func parseTerminalShellMetadataValue(value string) string {
	trimmed := strings.TrimSpace(value)
	if unquoted, err := strconv.Unquote(trimmed); err == nil {
		return unquoted
	}
	return trimmed
}

// terminalFilePathForShell names the terminal file for a shell id the way the
// client does: folder, a forward slash, "<id>.txt". The folder may live on a
// remote host with a different path style, so it is never cleaned or joined
// with this process's separator. Only numeric ids name terminal files.
func terminalFilePathForShell(terminalsFolder string, shellID string) (string, bool) {
	folder := strings.TrimSpace(terminalsFolder)
	id := strings.TrimSpace(shellID)
	if folder == "" || !isNumericShellID(id) {
		return "", false
	}
	return strings.TrimSuffix(folder, "/") + "/" + id + ".txt", true
}

func isNumericShellID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// terminalRuntimeSample is the last distinct running_for_ms value seen in the
// header and when it was read.
type terminalRuntimeSample struct {
	RunningForMS int64
	ReadAt       time.Time
}

// estimateShellRuntimeMS mirrors the client: the header counter (refreshed
// every few seconds) plus the time since it was read is always usable.
// started_at is only preferred when it agrees with that estimate, because the
// shell may run on another host whose clock differs from ours.
func estimateShellRuntimeMS(now time.Time, startedAt time.Time, sample terminalRuntimeSample) int64 {
	fromCounter := sample.RunningForMS + now.Sub(sample.ReadAt).Milliseconds()
	if startedAt.IsZero() {
		return fromCounter
	}
	fromClock := now.Sub(startedAt).Milliseconds()
	drift := fromClock - fromCounter
	if drift >= -1000 && drift <= 6000 {
		if fromClock > fromCounter {
			return fromClock
		}
		return fromCounter
	}
	return fromCounter
}
