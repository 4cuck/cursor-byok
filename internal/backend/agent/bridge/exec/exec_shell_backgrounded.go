package execbridge

import (
	"fmt"
	"strings"

	"cursor/gen/agentv1"
	runtimecore "cursor/internal/backend/agent/core"
)

// ShellBackgroundedText is the text form of the result of a Shell call that
// ends with its command still running in the background. The model is
// normally sent the Shell card's result (see buildShellBackgroundedToolCall);
// this text is what it is sent when that card cannot be used.
//
// It follows the Cursor client's own Shell tool. This result is the model's
// only introduction to the background job, so it says that the command is
// still running, names the shell (the id AwaitShell and WriteShellStdin take)
// and the terminal file its output goes to, and hands over what the command
// printed while it was still in the foreground.
//
// terminalFile is the file the client writes the output to. Pass "" when the
// terminals folder is not known; the file is then named the way the client
// names it in that case.
func ShellBackgroundedText(event *agentv1.ShellStreamBackgrounded, pending runtimecore.PendingExec, terminalFile string) string {
	shellID := event.GetShellId()
	file := strings.TrimSpace(terminalFile)
	if file == "" {
		file = fmt.Sprintf("<terminals_folder>/%d.txt", shellID)
	}
	args := decodeShellArgsForResult(pending.ArgsJSON)
	userRequested := event.GetReason() == agentv1.ShellBackgroundReason_SHELL_BACKGROUND_REASON_USER_REQUEST
	waited := event.MsToWait != nil
	// block_until_ms 0 asks for a background start; the client reports it as a
	// wait of 0ms.
	startedInBackground := !userRequested &&
		((args.BlockUntilMSSet && args.BlockUntilMS <= 0) || (waited && event.GetMsToWait() <= 0))

	var text strings.Builder
	identity := func() {
		fmt.Fprintf(&text, "Shell ID: %d\n", shellID)
		if pid := event.GetPid(); pid != 0 {
			fmt.Fprintf(&text, "PID: %d\n", pid)
		}
	}

	if startedInBackground {
		text.WriteString("Background command started successfully.\n")
		identity()
		if command := firstNonEmptyShellText(event.GetCommand(), args.Command); command != "" {
			text.WriteString("Command: " + command + "\n")
		}
		text.WriteString("Output will be written to " + file + ". Don't mention Shell ID to the user.")
		if section := shellCollectedOutputSection("Output collected so far", pending.StdoutBuffer, pending.StderrBuffer); section != "" {
			text.WriteString("\n\n" + section)
		}
		return text.String()
	}

	switch {
	case userRequested:
		text.WriteString("The user manually backgrounded the command")
		if waited {
			fmt.Fprintf(&text, " after %dms", event.GetMsToWait())
		}
		text.WriteString(". Do NOT await it; either continue with other work or end your turn.\n")
	case waited:
		fmt.Fprintf(&text, "The command did not complete in %dms and was sent to the background.\n", event.GetMsToWait())
	default:
		text.WriteString("The command exceeded block_until_ms and was sent to the background.\n")
	}
	identity()
	text.WriteString("The output is being written to " + file + ". Don't mention Shell ID to the user.\n\n")
	if section := shellCollectedOutputSection("Output collected before backgrounding", pending.StdoutBuffer, pending.StderrBuffer); section != "" {
		text.WriteString(section)
	} else {
		text.WriteString("No output was collected before backgrounding.")
	}
	return text.String()
}

// shellCollectedOutputSection renders the output a shell produced so far as a
// headed block, bounded the same way a finished Shell result is. It returns ""
// when nothing was printed.
func shellCollectedOutputSection(heading string, stdout string, stderr string) string {
	boundedStdout, boundedStderr := truncateShellStreamsForReplay(stdout, stderr)
	truncated := len(boundedStdout) != len(stdout) || len(boundedStderr) != len(stderr)
	trimmedStdout := trimShellOutputBlock(boundedStdout)
	trimmedStderr := trimShellOutputBlock(boundedStderr)
	var body string
	switch {
	case trimmedStdout != "" && trimmedStderr != "":
		body = trimmedStdout + "\n\n<stderr>\n" + trimmedStderr + "\n</stderr>"
	case trimmedStdout != "":
		body = trimmedStdout
	case trimmedStderr != "":
		body = trimmedStderr
	default:
		return ""
	}
	if truncated {
		heading += " (truncated)"
	}
	return heading + ":\n\n```\n" + body + "\n```"
}

// trimShellOutputBlock drops blank lines around the output and trailing
// whitespace, keeping the indentation of the first line.
func trimShellOutputBlock(output string) string {
	if strings.TrimSpace(output) == "" {
		return ""
	}
	return strings.TrimRight(strings.TrimLeft(output, "\r\n"), " \t\r\n")
}

func firstNonEmptyShellText(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
