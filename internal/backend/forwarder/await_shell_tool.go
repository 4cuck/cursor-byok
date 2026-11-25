// await_shell_tool.go implements AwaitShell: wait for a backgrounded shell, or
// simply sleep, and report what happened.
//
// It follows the Cursor client's own tool. The shell's terminal file is the
// source of truth: completion is the footer block, the pattern is matched
// against the output only, and the wait really blocks until the command ends,
// the pattern matches or block_until_ms runs out.
//
// The terminal file lives where the shell runs. For a local workspace this
// process reads it directly. For a remote workspace (WSL, SSH, containers) the
// path only exists on the other host, so it is read through the client's Read
// exec exactly like the client's tool does.
//
// A wait that has not returned yet owns a PendingExec, so the turn counts it as
// outstanding work: the provider is not resumed until the result is written.
package forwarder

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"cursor/gen/agentv1"
	execbridge "cursor/internal/backend/agent/bridge/exec"
	runtimecore "cursor/internal/backend/agent/core"
	"cursor/internal/logger"
)

const (
	awaitShellDefaultBlockMS int64 = 30_000
	// awaitShellMaxBlockMS is the client's per-call limit (119 minutes).
	awaitShellMaxBlockMS int64 = 7_140_000

	// awaitShellExecKind marks the PendingExec that represents the wait itself.
	awaitShellExecKind = "await_shell"
	// awaitShellReadExecKind marks one poll of a remote terminal file.
	awaitShellReadExecKind = "await_shell_read"

	awaitShellOutputTailLimit = 4 * 1024
	awaitShellMatchLimit      = 1000

	// A local terminal file is read whole up to this size; beyond it only the
	// header and the end are read, which is all completion detection needs.
	awaitShellLocalReadLimit = 16 << 20
	awaitShellLocalHeadBytes = 64 << 10
	awaitShellLocalTailBytes = 4 << 20

	// After the first full read, remote polls only ask for the end of the file
	// so a long log is not shipped through the client on every poll.
	awaitShellRemoteTailLines = 2000
	awaitShellRemoteFastPolls = 20
)

var (
	// awaitPollInterval matches the client's 250ms poll.
	awaitPollInterval = 250 * time.Millisecond
	// Long remote waits back off: each poll is a round trip through the client.
	awaitRemoteSlowPollInterval = time.Second
	// awaitRemoteReadTimeout is how long one remote poll may stay unanswered
	// before another one is sent.
	awaitRemoteReadTimeout = 15 * time.Second
	// awaitRemoteAnswerGrace lets a poll that is already on its way finish
	// before the deadline result is written.
	awaitRemoteAnswerGrace = 2 * time.Second
)

type awaitShellArgs struct {
	// ShellID is empty for a plain sleep.
	ShellID      string
	BlockUntilMS int64
	Pattern      string
	pattern      *regexp.Regexp
}

// awaitShellResult is what the model reads.
type awaitShellResult struct {
	ShellID        string  `json:"shell_id,omitempty"`
	Status         string  `json:"status"`
	Message        string  `json:"message,omitempty"`
	ExitCode       *int64  `json:"exit_code,omitempty"`
	Matched        bool    `json:"matched"`
	TimedOut       bool    `json:"timed_out"`
	RuntimeMS      uint64  `json:"runtime_ms,omitempty"`
	OutputFilePath string  `json:"output_file_path,omitempty"`
	OutputLength   uint64  `json:"output_length,omitempty"`
	RegexRequested bool    `json:"regex_requested,omitempty"`
	RegexMatch     *string `json:"regex_match,omitempty"`
	OutputTail     string  `json:"output_tail,omitempty"`

	complete  bool
	errorText string
}

// awaitShellObservation is one look at a shell, from whichever source could
// see it.
type awaitShellObservation struct {
	Exists   bool
	Complete bool
	// EndStatus is set when the stream reported an end other than a normal exit.
	EndStatus    string
	ExitCode     *int32
	ElapsedMS    *int64
	StartedAt    time.Time
	RunningForMS *int64
	// RuntimeMS is set by sources that have no header timing of their own.
	RuntimeMS    *int64
	FooterError  string
	Body         string
	OutputLength int64
}

// pendingAwaitShell is one AwaitShell call that has not returned yet. Fields
// below Deadline change between polls and are guarded by stream.mu.
type pendingAwaitShell struct {
	Key        string
	Invocation runtimecore.ToolInvocation
	Args       awaitShellArgs
	ExecID     string
	StartedAt  time.Time
	Deadline   time.Time

	// FilePath is the shell's terminal file as the client names it; empty when
	// the request carried no terminals folder.
	FilePath string
	// LocalReadable is true when this process can read FilePath itself.
	LocalReadable bool

	Polls      int
	ReadExecID string
	ReadSentAt time.Time

	last          awaitShellObservation
	observed      bool
	sample        terminalRuntimeSample
	hasSample     bool
	headerStarted time.Time
}

func awaitShellTimerKey(callID string) string {
	return "await_shell:" + strings.TrimSpace(callID)
}

func awaitShellExecID(callID string) string {
	return "exec-await-shell-" + strings.TrimSpace(callID)
}

// isAwaitShellExecKind reports whether a pending exec belongs to an AwaitShell
// wait. These never reach the client as tool executions of their own and are
// bounded by the wait's deadline, so generic exec watchdogs leave them alone.
func isAwaitShellExecKind(kind string) bool {
	switch strings.TrimSpace(kind) {
	case awaitShellExecKind, awaitShellReadExecKind:
		return true
	default:
		return false
	}
}

func (service *Service) handleAwaitShellToolInvocation(stream *ActiveStream, invocation runtimecore.ToolInvocation) error {
	args, err := decodeAwaitShellArgs(invocation.ArgsJSON)
	if err != nil {
		return service.completeAwaitShell(stream, invocation, args, awaitShellResult{
			ShellID:   args.ShellID,
			Status:    "error",
			Message:   err.Error(),
			errorText: err.Error(),
		})
	}
	now := time.Now()
	key := strings.TrimSpace(invocation.CallID)
	if key == "" {
		key = execbridge.NewExecID("anonymous-")
	}
	wait := &pendingAwaitShell{
		Key:        key,
		Invocation: invocation,
		Args:       args,
		ExecID:     awaitShellExecID(key),
		StartedAt:  now,
		Deadline:   now.Add(time.Duration(args.BlockUntilMS) * time.Millisecond),
	}
	if args.ShellID == "" {
		service.registerAwaitShell(stream, wait)
		service.scheduleAwaitShellTick(stream, wait, time.Until(wait.Deadline))
		return nil
	}

	stream.mu.Lock()
	terminalsFolder := strings.TrimSpace(stream.TerminalsFolder)
	stream.mu.Unlock()
	wait.FilePath, _ = terminalFilePathForShell(terminalsFolder, args.ShellID)
	wait.LocalReadable = wait.FilePath != "" && localDirectoryExists(terminalsFolder)

	service.registerAwaitShell(stream, wait)
	if wait.isRemote() {
		if observation, ok := observeShellFromStream(stream, args.ShellID); ok && observation.Complete {
			return service.applyAwaitShellObservation(stream, wait, observation, now)
		}
		return service.dispatchAwaitShellRead(stream, wait, now)
	}
	return service.applyAwaitShellObservation(stream, wait, service.observeAwaitShellHere(stream, wait), now)
}

func (wait *pendingAwaitShell) isRemote() bool {
	return wait.FilePath != "" && !wait.LocalReadable
}

// registerAwaitShell records the wait and the PendingExec that makes the turn
// treat it as outstanding work.
func (service *Service) registerAwaitShell(stream *ActiveStream, wait *pendingAwaitShell) {
	now := time.Now().UTC()
	stream.mu.Lock()
	if stream.PendingAwaits == nil {
		stream.PendingAwaits = make(map[string]*pendingAwaitShell)
	}
	if stream.PendingExecs == nil {
		stream.PendingExecs = make(map[string]runtimecore.PendingExec)
	}
	stream.PendingAwaits[wait.Key] = wait
	stream.PendingExecs[wait.ExecID] = runtimecore.PendingExec{
		ExecID:                   wait.ExecID,
		ExecKind:                 awaitShellExecKind,
		ProviderPass:             stream.ProviderPassCount,
		ModelCallID:              wait.Invocation.ModelCallID,
		ToolCallID:               wait.Key,
		ArgsJSON:                 append([]byte(nil), wait.Invocation.ArgsJSON...),
		ReasoningContent:         wait.Invocation.ReasoningContent,
		ReasoningSignature:       wait.Invocation.ReasoningSignature,
		ReasoningSignatureSource: wait.Invocation.ReasoningSignatureSource,
		StreamState:              "waiting",
		OpenedAt:                 now,
	}
	stream.UpdatedAt = now
	stream.mu.Unlock()
}

// claimAwaitShellLocked removes the wait and everything it registered. Only
// the caller that gets true may write the result, so a wait is answered once.
func claimAwaitShellLocked(stream *ActiveStream, wait *pendingAwaitShell) bool {
	if current, ok := stream.PendingAwaits[wait.Key]; !ok || current != wait {
		return false
	}
	delete(stream.PendingAwaits, wait.Key)
	delete(stream.PendingExecs, wait.ExecID)
	retireAwaitShellReadLocked(stream, wait)
	stopStreamTimerLocked(stream, awaitShellTimerKey(wait.Key))
	stream.UpdatedAt = time.Now().UTC()
	return true
}

// retireAwaitShellReadLocked forgets the poll that is still on its way. Its
// answer may yet arrive; the tombstone makes that a no-op.
func retireAwaitShellReadLocked(stream *ActiveStream, wait *pendingAwaitShell) {
	if wait.ReadExecID == "" {
		return
	}
	if read, ok := stream.PendingExecs[wait.ReadExecID]; ok {
		markExecCompletedLocked(stream, read, time.Now().UTC())
	}
	wait.ReadExecID = ""
}

// dropAwaitShellsLocked forgets every unanswered wait. It is used when the
// turn itself is torn down (cancel, new turn): no result must be written then.
func dropAwaitShellsLocked(stream *ActiveStream) {
	for key, wait := range stream.PendingAwaits {
		stopStreamTimerLocked(stream, awaitShellTimerKey(key))
		wait.ReadExecID = ""
		delete(stream.PendingAwaits, key)
	}
}

func stopStreamTimerLocked(stream *ActiveStream, key string) {
	if timer := stream.StreamTimers[key]; timer != nil {
		timer.Stop()
		delete(stream.StreamTimers, key)
	}
	delete(stream.TimerTokens, key)
}

func (service *Service) scheduleAwaitShellTick(stream *ActiveStream, wait *pendingAwaitShell, delay time.Duration) {
	if delay < 0 {
		delay = 0
	}
	service.scheduleStreamTimer(stream, awaitShellTimerKey(wait.Key), delay, streamTimerAwaitShell, "", 0, "await_shell_poll")
}

// handleAwaitShellTimer runs one poll of a wait that has not returned yet.
func (service *Service) handleAwaitShellTimer(stream *ActiveStream, payload *streamTimerEvent) error {
	if stream == nil || payload == nil {
		return nil
	}
	key := strings.TrimPrefix(strings.TrimSpace(payload.Key), "await_shell:")
	now := time.Now()

	stream.mu.Lock()
	wait, ok := stream.PendingAwaits[key]
	if !ok {
		stream.mu.Unlock()
		return nil
	}
	_, stillPending := stream.PendingExecs[wait.ExecID]
	if isTerminalStreamStatus(stream.Status) || !stillPending {
		// The turn ended or dropped its pending work; there is nothing left to
		// answer.
		delete(stream.PendingAwaits, key)
		stopStreamTimerLocked(stream, awaitShellTimerKey(key))
		stream.mu.Unlock()
		return nil
	}
	sleepOnly := wait.Args.ShellID == ""
	stream.mu.Unlock()

	switch {
	case sleepOnly:
		if now.Before(wait.Deadline) {
			service.scheduleAwaitShellTick(stream, wait, time.Until(wait.Deadline))
			return nil
		}
		return service.finishAwaitShell(stream, wait, awaitShellSleepResult(wait))
	case wait.isRemote():
		return service.tickRemoteAwaitShell(stream, wait, now)
	default:
		return service.applyAwaitShellObservation(stream, wait, service.observeAwaitShellHere(stream, wait), now)
	}
}

// applyAwaitShellObservation decides whether the wait is over. If it is, the
// result is written; otherwise the next poll is scheduled.
func (service *Service) applyAwaitShellObservation(stream *ActiveStream, wait *pendingAwaitShell, observation awaitShellObservation, now time.Time) error {
	match := awaitShellMatch(wait.Args, observation.Body)

	stream.mu.Lock()
	if current, ok := stream.PendingAwaits[wait.Key]; !ok || current != wait {
		stream.mu.Unlock()
		return nil
	}
	result, final := wait.evaluateLocked(observation, match, now)
	if final {
		claimAwaitShellLocked(stream, wait)
	}
	delay := wait.nextPollDelayLocked(now)
	stream.mu.Unlock()

	if final {
		return service.completeAwaitShell(stream, wait.Invocation, wait.Args, result)
	}
	service.scheduleAwaitShellTick(stream, wait, delay)
	return nil
}

// finishAwaitShell writes a result that does not depend on a fresh
// observation (sleep finished, read failed, deadline reached).
func (service *Service) finishAwaitShell(stream *ActiveStream, wait *pendingAwaitShell, result awaitShellResult) error {
	stream.mu.Lock()
	claimed := claimAwaitShellLocked(stream, wait)
	stream.mu.Unlock()
	if !claimed {
		return nil
	}
	return service.completeAwaitShell(stream, wait.Invocation, wait.Args, result)
}

func (service *Service) completeAwaitShell(stream *ActiveStream, invocation runtimecore.ToolInvocation, args awaitShellArgs, result awaitShellResult) error {
	payload, err := json.Marshal(result)
	if err != nil {
		return err
	}
	toolCall := buildAwaitShellToolCall(buildAwaitArgsFromAwaitShellArgs(args), buildAwaitShellProtoResult(result))
	return service.completeImmediateToolResult(stream, invocation, string(payload), toolCall)
}

// evaluateLocked folds one observation into the wait and reports whether the
// wait is over. Caller holds stream.mu.
func (wait *pendingAwaitShell) evaluateLocked(observation awaitShellObservation, match string, now time.Time) (awaitShellResult, bool) {
	if !observation.Exists {
		return awaitShellFailure(wait.Args, fmt.Sprintf("No shell found for id %s", wait.Args.ShellID)), true
	}
	if observation.RunningForMS != nil && (!wait.hasSample || wait.sample.RunningForMS != *observation.RunningForMS) {
		wait.sample = terminalRuntimeSample{RunningForMS: *observation.RunningForMS, ReadAt: now}
		wait.hasSample = true
	}
	if !observation.StartedAt.IsZero() {
		wait.headerStarted = observation.StartedAt
	}
	wait.observed = true
	wait.last = observation
	wait.last.Body = awaitShellOutputTail(observation.Body)

	timedOut := false
	switch {
	case observation.Complete, match != "", wait.Args.BlockUntilMS == 0:
	case !now.Before(wait.Deadline):
		timedOut = true
	default:
		return awaitShellResult{}, false
	}
	return wait.buildResultLocked(observation, match, timedOut, now), true
}

func (wait *pendingAwaitShell) buildResultLocked(observation awaitShellObservation, match string, timedOut bool, now time.Time) awaitShellResult {
	runtime := wait.runtimeMSLocked(observation, now)
	result := awaitShellResult{
		ShellID:        wait.Args.ShellID,
		TimedOut:       timedOut,
		RuntimeMS:      uint64(runtime),
		OutputFilePath: wait.FilePath,
		RegexRequested: wait.Args.pattern != nil,
		OutputTail:     awaitShellOutputTail(observation.Body),
	}
	if observation.OutputLength > 0 {
		result.OutputLength = uint64(observation.OutputLength)
	}
	patternNote := ""
	if result.RegexRequested {
		patternNote = "Pattern did NOT match."
		if match != "" {
			shown := truncateAwaitShellMatch(match)
			result.Matched = true
			result.RegexMatch = &shown
			patternNote = strings.TrimRight("Pattern matched: "+shown, " \t\r\n")
		}
	}

	if observation.Complete {
		result.complete = true
		result.Status = backgroundShellStatusCompleted
		exitText := "unknown"
		if observation.ExitCode != nil {
			code := int64(*observation.ExitCode)
			result.ExitCode = &code
			exitText = strconv.FormatInt(code, 10)
		}
		result.Message = fmt.Sprintf("Task completed in %dms with exit code: %s.", runtime, exitText)
		if endStatus := strings.TrimSpace(observation.EndStatus); endStatus != "" {
			result.Status = endStatus
			result.Message = fmt.Sprintf("Task ended after %dms without running to completion (%s).", runtime, endStatus)
		}
		if footerError := strings.TrimSpace(observation.FooterError); footerError != "" {
			result.Message += " " + footerError
		}
	} else {
		result.Status = backgroundShellStatusRunning
		result.Message = fmt.Sprintf("Task still running after %dms...", runtime)
	}
	if patternNote != "" {
		result.Message += " " + patternNote
	}
	return result
}

func (wait *pendingAwaitShell) runtimeMSLocked(observation awaitShellObservation, now time.Time) int64 {
	var runtime int64
	switch {
	case observation.RuntimeMS != nil:
		runtime = *observation.RuntimeMS
	case observation.Complete && observation.ElapsedMS != nil:
		runtime = *observation.ElapsedMS
	case observation.Complete && wait.hasSample:
		runtime = wait.sample.RunningForMS
	case wait.hasSample:
		runtime = estimateShellRuntimeMS(now, wait.headerStarted, wait.sample)
	default:
		runtime = now.Sub(wait.StartedAt).Milliseconds()
	}
	if runtime < 0 {
		runtime = 0
	}
	return runtime
}

// nextPollDelayLocked is the time until the next poll, never past the
// deadline. Caller holds stream.mu.
func (wait *pendingAwaitShell) nextPollDelayLocked(now time.Time) time.Duration {
	delay := awaitPollInterval
	if wait.isRemote() && wait.Polls > awaitShellRemoteFastPolls {
		delay = awaitRemoteSlowPollInterval
	}
	if remaining := wait.Deadline.Sub(now); remaining < delay {
		delay = remaining
	}
	if delay < 0 {
		delay = 0
	}
	return delay
}

func awaitShellSleepResult(wait *pendingAwaitShell) awaitShellResult {
	slept := wait.Args.BlockUntilMS
	message := "Slept briefly."
	if slept > 0 {
		seconds := int64(math.Ceil(float64(slept) / 1000))
		if seconds < 1 {
			seconds = 1
		}
		message = fmt.Sprintf("Slept for %ds.", seconds)
	}
	return awaitShellResult{
		Status:    "waited",
		Message:   message,
		RuntimeMS: uint64(slept),
		complete:  true,
	}
}

func awaitShellFailure(args awaitShellArgs, reason string) awaitShellResult {
	return awaitShellResult{
		ShellID:   args.ShellID,
		Status:    "error",
		Message:   "Error awaiting task: " + reason,
		errorText: reason,
	}
}

// ---------------------------------------------------------------------------
// Observation: local file, stream state
// ---------------------------------------------------------------------------

// observeAwaitShellHere looks at the shell with what this process can see: the
// terminal file when it is on this machine, otherwise what the stream reported.
func (service *Service) observeAwaitShellHere(stream *ActiveStream, wait *pendingAwaitShell) awaitShellObservation {
	fromStream, hasStream := observeShellFromStream(stream, wait.Args.ShellID)
	if wait.LocalReadable {
		if fromFile, ok := observeTerminalFileLocally(wait.FilePath); ok {
			return mergeStreamCompletion(fromFile, fromStream, hasStream)
		}
	}
	if hasStream {
		return fromStream
	}
	return awaitShellObservation{}
}

// mergeStreamCompletion trusts an end the stream already reported even when
// the terminal file's footer has not been written yet.
func mergeStreamCompletion(fromFile awaitShellObservation, fromStream awaitShellObservation, hasStream bool) awaitShellObservation {
	if !fromFile.Complete && hasStream && fromStream.Complete {
		fromFile.Complete = true
		fromFile.EndStatus = fromStream.EndStatus
		fromFile.ExitCode = fromStream.ExitCode
	}
	return fromFile
}

func observeShellFromStream(stream *ActiveStream, shellID string) (awaitShellObservation, bool) {
	if stream == nil {
		return awaitShellObservation{}, false
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	state := stream.BackgroundShells[strings.TrimSpace(shellID)]
	if state == nil {
		return awaitShellObservation{}, false
	}
	body := state.StdoutBuffer
	if state.StderrBuffer != "" {
		if body != "" && !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		body += state.StderrBuffer
	}
	runtime := int64(backgroundShellRuntimeMS(state.CreatedAt, state.CompletedAt))
	observation := awaitShellObservation{
		Exists:       true,
		RuntimeMS:    &runtime,
		Body:         body,
		OutputLength: int64(len(body)),
	}
	if isBackgroundShellTerminalStatus(state.Status) {
		observation.Complete = true
		if strings.TrimSpace(state.Status) != backgroundShellStatusCompleted {
			observation.EndStatus = strings.TrimSpace(state.Status)
		}
		if state.ExitCode != nil {
			code := *state.ExitCode
			observation.ExitCode = &code
		}
	}
	return observation, true
}

func backgroundShellRuntimeMS(createdAt time.Time, completedAt time.Time) uint64 {
	if createdAt.IsZero() {
		return 0
	}
	end := completedAt
	if end.IsZero() {
		end = time.Now().UTC()
	}
	if end.Before(createdAt) {
		return 0
	}
	return uint64(end.Sub(createdAt).Milliseconds())
}

// localDirectoryExists reports whether folder is a directory on this machine.
// A path from another host's file system is not absolute here (Windows) or
// simply does not exist (POSIX), and is then read through the client instead.
func localDirectoryExists(folder string) bool {
	if !filepath.IsAbs(folder) {
		return false
	}
	info, err := os.Stat(folder)
	return err == nil && info.IsDir()
}

func observeTerminalFileLocally(path string) (awaitShellObservation, bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return awaitShellObservation{}, false
	}
	size := info.Size()
	if size <= awaitShellLocalReadLimit {
		data, err := os.ReadFile(path)
		if err != nil {
			return awaitShellObservation{}, false
		}
		return observationFromTerminalView(parseTerminalFileView(string(data)), size), true
	}
	head, tail, err := readFileHeadAndTail(path, size, awaitShellLocalHeadBytes, awaitShellLocalTailBytes)
	if err != nil {
		return awaitShellObservation{}, false
	}
	// The tail starts mid-line; begin at the next full line.
	if newline := strings.IndexByte(tail, '\n'); newline >= 0 {
		tail = tail[newline+1:]
	}
	view := parseTerminalFileView(tail)
	header := parseTerminalFileView(head)
	view.StartedAt = header.StartedAt
	view.RunningForMS = header.RunningForMS
	view.Status = header.Status
	return observationFromTerminalView(view, size), true
}

func readFileHeadAndTail(path string, size int64, headBytes int64, tailBytes int64) (string, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer file.Close()
	head := make([]byte, headBytes)
	read, err := io.ReadFull(file, head)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return "", "", err
	}
	head = head[:read]
	offset := size - tailBytes
	if offset < 0 {
		offset = 0
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return "", "", err
	}
	tail, err := io.ReadAll(file)
	if err != nil {
		return "", "", err
	}
	return string(head), string(tail), nil
}

func observationFromTerminalView(view terminalFileView, size int64) awaitShellObservation {
	return awaitShellObservation{
		Exists:       true,
		Complete:     view.Complete,
		ExitCode:     view.ExitCode,
		ElapsedMS:    view.ElapsedMS,
		StartedAt:    view.StartedAt,
		RunningForMS: view.RunningForMS,
		FooterError:  view.FooterError,
		Body:         view.Body,
		OutputLength: size,
	}
}

// ---------------------------------------------------------------------------
// Observation: remote file through the client's Read exec
// ---------------------------------------------------------------------------

// tickRemoteAwaitShell drives a wait whose terminal file only the client can
// read: send a poll, give an outstanding one time to answer, or finish at the
// deadline with the last thing the client reported.
func (service *Service) tickRemoteAwaitShell(stream *ActiveStream, wait *pendingAwaitShell, now time.Time) error {
	if observation, ok := observeShellFromStream(stream, wait.Args.ShellID); ok && observation.Complete {
		return service.applyAwaitShellObservation(stream, wait, observation, now)
	}

	stream.mu.Lock()
	if current, ok := stream.PendingAwaits[wait.Key]; !ok || current != wait {
		stream.mu.Unlock()
		return nil
	}
	expired := !now.Before(wait.Deadline)
	outstanding := wait.ReadExecID != ""
	readSentAt := wait.ReadSentAt
	readAge := now.Sub(readSentAt)
	var deadlineResult awaitShellResult
	finishNow := expired && (!outstanding || readAge >= awaitRemoteAnswerGrace)
	if finishNow {
		last := wait.last
		if !wait.observed {
			// The client never answered. The shell id was handed out by the
			// client, so report it as still running rather than as unknown.
			last = awaitShellObservation{Exists: true}
		}
		deadlineResult = wait.buildResultLocked(last, "", wait.Args.BlockUntilMS > 0, now)
		if !wait.observed {
			deadlineResult.Message += " The client did not report the terminal file in time; read output_file_path to check."
		}
	}
	abandonStale := !finishNow && outstanding && readAge >= awaitRemoteReadTimeout
	if abandonStale {
		retireAwaitShellReadLocked(stream, wait)
		outstanding = false
	}
	stream.mu.Unlock()

	switch {
	case finishNow:
		return service.finishAwaitShell(stream, wait, deadlineResult)
	case outstanding:
		// Wake up again when the poll is overdue or the deadline needs a result.
		wakeAt := readSentAt.Add(awaitRemoteReadTimeout)
		if expired {
			wakeAt = readSentAt.Add(awaitRemoteAnswerGrace)
		} else if wait.Deadline.Before(wakeAt) {
			wakeAt = wait.Deadline
		}
		service.scheduleAwaitShellTick(stream, wait, wakeAt.Sub(now))
		return nil
	default:
		return service.dispatchAwaitShellRead(stream, wait, now)
	}
}

// dispatchAwaitShellRead asks the client to read the terminal file. The first
// poll reads the whole file (header, and any output a pattern may already
// match); later polls only read the end, where new output and the footer are.
func (service *Service) dispatchAwaitShellRead(stream *ActiveStream, wait *pendingAwaitShell, now time.Time) error {
	stream.mu.Lock()
	if current, ok := stream.PendingAwaits[wait.Key]; !ok || current != wait {
		stream.mu.Unlock()
		return nil
	}
	firstPoll := wait.Polls == 0
	conversationID := stream.ConversationID
	providerPass := stream.ProviderPassCount
	stream.mu.Unlock()

	readArgs := map[string]any{"path": wait.FilePath}
	if !firstPoll {
		readArgs["offset"] = -awaitShellRemoteTailLines
	}
	readArgsJSON, err := json.Marshal(readArgs)
	if err != nil {
		return err
	}
	serverMessage, pendingExec, err := service.execBridge.OpenExec(execbridge.OpenExecContext{
		ConversationID: conversationID,
	}, runtimecore.ToolInvocation{
		CallID:      wait.Key,
		ToolName:    "Read",
		ArgsJSON:    readArgsJSON,
		ModelCallID: wait.Invocation.ModelCallID,
	})
	if err != nil {
		return service.finishAwaitShell(stream, wait, awaitShellFailure(wait.Args, "Failed to read shell output: "+err.Error()))
	}
	pendingExec.ExecKind = awaitShellReadExecKind
	pendingExec.ToolCallID = wait.Key
	pendingExec.ModelCallID = wait.Invocation.ModelCallID
	pendingExec.ProviderPass = providerPass

	stream.mu.Lock()
	if current, ok := stream.PendingAwaits[wait.Key]; !ok || current != wait {
		stream.mu.Unlock()
		return nil
	}
	stream.PendingExecs[pendingExec.ExecID] = pendingExec
	wait.ReadExecID = pendingExec.ExecID
	wait.ReadSentAt = now
	wait.Polls++
	stream.mu.Unlock()

	if err := service.broker.Publish(stream.RequestID, StreamEvent{Message: serverMessage}); err != nil {
		return err
	}
	// Whatever happens to this poll, wake up for the deadline or to retry.
	wakeAt := now.Add(awaitRemoteReadTimeout)
	if wait.Deadline.After(now) && wait.Deadline.Before(wakeAt) {
		wakeAt = wait.Deadline
	} else if !wait.Deadline.After(now) {
		wakeAt = now.Add(awaitRemoteAnswerGrace)
	}
	service.scheduleAwaitShellTick(stream, wait, wakeAt.Sub(now))
	return nil
}

// handleAwaitShellReadResult consumes the client's answer to one poll. Polls
// are internal: they never become history entries or tool cards.
func (service *Service) handleAwaitShellReadResult(stream *ActiveStream, pending runtimecore.PendingExec, message *agentv1.ExecClientMessage) error {
	markExecCompleted(stream, pending)
	now := time.Now()

	stream.mu.Lock()
	wait, ok := stream.PendingAwaits[strings.TrimSpace(pending.ToolCallID)]
	if !ok || wait.ReadExecID != pending.ExecID {
		// An answer to a poll that was given up on, or to a wait that is over.
		stream.mu.Unlock()
		return nil
	}
	wait.ReadExecID = ""
	stream.mu.Unlock()

	observation, failure := awaitShellObservationFromRead(message.GetReadResult(), wait.FilePath)
	if failure != "" {
		return service.finishAwaitShell(stream, wait, awaitShellFailure(wait.Args, failure))
	}
	if fromStream, hasStream := observeShellFromStream(stream, wait.Args.ShellID); hasStream {
		if observation.Exists {
			observation = mergeStreamCompletion(observation, fromStream, true)
		} else {
			// No terminal file, but the stream itself reported this shell.
			observation = fromStream
		}
	}
	return service.applyAwaitShellObservation(stream, wait, observation, now)
}

// handleAwaitShellReadControl handles control messages for a poll. Heartbeats
// and the closing ack carry no information; a throw means the read failed.
func (service *Service) handleAwaitShellReadControl(stream *ActiveStream, pending runtimecore.PendingExec, message *agentv1.ExecClientControlMessage) error {
	thrown := message.GetThrow()
	if thrown == nil {
		return nil
	}
	markExecCompleted(stream, pending)
	stream.mu.Lock()
	wait, ok := stream.PendingAwaits[strings.TrimSpace(pending.ToolCallID)]
	if !ok || wait.ReadExecID != pending.ExecID {
		stream.mu.Unlock()
		return nil
	}
	wait.ReadExecID = ""
	stream.mu.Unlock()
	reason := strings.TrimSpace(thrown.GetError())
	if reason == "" {
		reason = "the client could not read the terminal file"
	}
	logger.Infof("forwarder await shell read failed request_id=%s tool_call_id=%s err=%s", strings.TrimSpace(stream.RequestID), strings.TrimSpace(wait.Key), reason)
	return service.finishAwaitShell(stream, wait, awaitShellFailure(wait.Args, "Failed to read shell output: "+reason))
}

// awaitShellObservationFromRead turns a Read result into an observation. A
// non-empty second value is a failure that ends the wait.
func awaitShellObservationFromRead(result *agentv1.ReadResult, path string) (awaitShellObservation, string) {
	switch item := result.GetResult().(type) {
	case *agentv1.ReadResult_Success:
		content := item.Success.GetContent()
		if content == "" && len(item.Success.GetData()) > 0 {
			content = string(item.Success.GetData())
		}
		size := item.Success.GetFileSize()
		if size <= 0 {
			size = int64(len(content))
		}
		return observationFromTerminalView(parseTerminalFileView(content), size), ""
	case *agentv1.ReadResult_FileNotFound:
		return awaitShellObservation{}, ""
	case *agentv1.ReadResult_PermissionDenied:
		return awaitShellObservation{}, "Permission denied reading " + path
	case *agentv1.ReadResult_Rejected:
		return awaitShellObservation{}, firstNonEmpty(strings.TrimSpace(item.Rejected.GetReason()), "Read rejected for "+path)
	case *agentv1.ReadResult_InvalidFile:
		return awaitShellObservation{}, firstNonEmpty(strings.TrimSpace(item.InvalidFile.GetReason()), "Invalid file: "+path)
	case *agentv1.ReadResult_Error:
		return awaitShellObservation{}, firstNonEmpty(strings.TrimSpace(item.Error.GetError()), "Read failed for "+path)
	default:
		return awaitShellObservation{}, "Unknown read result for " + path
	}
}

// ---------------------------------------------------------------------------
// Arguments, pattern, output
// ---------------------------------------------------------------------------

// decodeAwaitShellArgs normalizes arguments the way the client does: ids may
// be numbers or strings, "none" means no shell, and block_until_ms falls back
// to the default when missing, negative or not a number. Partially decoded
// arguments are returned alongside an error so the tool card can still show
// them.
func decodeAwaitShellArgs(raw []byte) (awaitShellArgs, error) {
	args := awaitShellArgs{BlockUntilMS: awaitShellDefaultBlockMS}
	argsMap, err := runtimecore.DecodeArgsMap(raw)
	if err != nil {
		return args, fmt.Errorf("decode AwaitShell args failed: %w", err)
	}
	args.ShellID = awaitShellIDArg(argsMap)
	args.BlockUntilMS = awaitShellBlockArg(argsMap)
	args.Pattern = strings.TrimSpace(runtimecore.ReadStringArg(argsMap, "pattern", "regex"))
	if args.Pattern != "" {
		compiled, compileErr := cachedRegexpCompile("(?m)" + args.Pattern)
		if compileErr != nil {
			return args, fmt.Errorf("pattern is not valid RE2 syntax: %v. RE2 has no lookahead, lookbehind or backreferences", compileErr)
		}
		args.pattern = compiled
	}
	if args.ShellID == "" && args.BlockUntilMS == 0 {
		return args, fmt.Errorf("Invalid arguments:\nshell_id: Required when block_until_ms is 0. Without shell_id there is nothing to check: pass the shell_id to check, or a nonzero block_until_ms to sleep")
	}
	return args, nil
}

func awaitShellIDArg(args map[string]any) string {
	for _, key := range []string{"shell_id", "task_id", "shellId", "taskId"} {
		value, ok := args[key]
		if !ok || value == nil {
			continue
		}
		switch item := value.(type) {
		case string:
			return normalizeAwaitShellID(item)
		case json.Number:
			return normalizeAwaitShellID(integerText(item.String()))
		case float64:
			return normalizeAwaitShellID(integerText(strconv.FormatFloat(item, 'f', -1, 64)))
		}
	}
	return ""
}

// integerText renders a whole-valued number literal without a fraction
// ("447245.0" -> "447245"); anything else is returned unchanged.
func integerText(literal string) string {
	if _, err := strconv.ParseInt(literal, 10, 64); err == nil {
		return literal
	}
	value, err := strconv.ParseFloat(literal, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value != math.Trunc(value) || math.Abs(value) >= 1e15 {
		return literal
	}
	return strconv.FormatInt(int64(value), 10)
}

func normalizeAwaitShellID(value string) string {
	trimmed := strings.TrimSpace(value)
	if strings.EqualFold(trimmed, "none") {
		return ""
	}
	return trimmed
}

func awaitShellBlockArg(args map[string]any) int64 {
	value, found, err := runtimecore.ReadFloat64Arg(args, "block_until_ms", "blockUntilMs", "blockUntilMS")
	if err != nil || !found || value < 0 {
		return awaitShellDefaultBlockMS
	}
	if value >= float64(awaitShellMaxBlockMS) {
		return awaitShellMaxBlockMS
	}
	return int64(math.Floor(value))
}

// awaitShellMatch returns the first match of the pattern in the output. An
// empty match counts as no match, as it does in the client.
func awaitShellMatch(args awaitShellArgs, body string) string {
	if args.pattern == nil || body == "" {
		return ""
	}
	return args.pattern.FindString(body)
}

func truncateAwaitShellMatch(match string) string {
	if len(match) <= awaitShellMatchLimit {
		return match
	}
	half := awaitShellMatchLimit / 2
	head := half
	for head > 0 && !utf8.RuneStart(match[head]) {
		head--
	}
	tail := len(match) - half
	for tail < len(match) && !utf8.RuneStart(match[tail]) {
		tail++
	}
	return match[:head] + "..." + match[tail:]
}

// awaitShellOutputTail keeps the end of the output, starting on a line
// boundary when one is close.
func awaitShellOutputTail(body string) string {
	trimmed := strings.Trim(body, "\r\n")
	if len(trimmed) <= awaitShellOutputTailLimit {
		return trimmed
	}
	cut := len(trimmed) - awaitShellOutputTailLimit
	for cut < len(trimmed) && !utf8.RuneStart(trimmed[cut]) {
		cut++
	}
	if newline := strings.IndexByte(trimmed[cut:], '\n'); newline >= 0 && newline < 512 {
		cut += newline + 1
	}
	return trimmed[cut:]
}

// ---------------------------------------------------------------------------
// Tool card
// ---------------------------------------------------------------------------

func buildAwaitArgsFromAwaitShellArgs(args awaitShellArgs) *agentv1.AwaitArgs {
	block := uint32(0)
	if args.BlockUntilMS > 0 {
		block = uint32(args.BlockUntilMS)
	}
	awaitArgs := &agentv1.AwaitArgs{
		TaskId:       strings.TrimSpace(args.ShellID),
		BlockUntilMs: &block,
	}
	if pattern := strings.TrimSpace(args.Pattern); pattern != "" {
		awaitArgs.Regex = &pattern
	}
	return awaitArgs
}

func buildAwaitShellToolCall(args *agentv1.AwaitArgs, result *agentv1.AwaitResult) *agentv1.ToolCall {
	return &agentv1.ToolCall{
		Tool: &agentv1.ToolCall_AwaitToolCall{
			AwaitToolCall: &agentv1.AwaitToolCall{
				Args:   args,
				Result: result,
			},
		},
	}
}

// buildAwaitShellProtoResult uses the client's result shape: success wrapping
// complete or still_running, or a top-level error.
func buildAwaitShellProtoResult(result awaitShellResult) *agentv1.AwaitResult {
	if strings.TrimSpace(result.Status) == "error" {
		reason := firstNonEmpty(strings.TrimSpace(result.errorText), strings.TrimSpace(result.Message), "unknown error")
		return &agentv1.AwaitResult{Result: &agentv1.AwaitResult_Error{Error: &agentv1.AwaitError{Error: reason}}}
	}
	if result.complete {
		task := &agentv1.AwaitTaskComplete{
			TaskId:         strings.TrimSpace(result.ShellID),
			RuntimeMs:      result.RuntimeMS,
			OutputFilePath: result.OutputFilePath,
			OutputLength:   result.OutputLength,
			RegexRequested: result.RegexRequested,
			RegexMatch:     result.RegexMatch,
		}
		if result.ExitCode != nil {
			exitCode := int32(*result.ExitCode)
			task.ExitCode = &exitCode
		}
		return &agentv1.AwaitResult{Result: &agentv1.AwaitResult_Success{Success: &agentv1.AwaitSuccess{
			AwaitResult: &agentv1.AwaitSuccess_Complete{Complete: task},
		}}}
	}
	task := &agentv1.AwaitTaskStillRunning{
		TaskId:         strings.TrimSpace(result.ShellID),
		RuntimeMs:      result.RuntimeMS,
		OutputFilePath: result.OutputFilePath,
		OutputLength:   result.OutputLength,
		RegexRequested: result.RegexRequested,
		RegexMatch:     result.RegexMatch,
	}
	return &agentv1.AwaitResult{Result: &agentv1.AwaitResult_Success{Success: &agentv1.AwaitSuccess{
		AwaitResult: &agentv1.AwaitSuccess_StillRunning{StillRunning: task},
	}}}
}
