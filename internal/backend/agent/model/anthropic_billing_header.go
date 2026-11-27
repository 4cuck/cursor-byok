package modeladapter

import (
	"crypto/sha256"
	"encoding/hex"

	"cursor/internal/modelchannel"
)

// The Claude CLI puts a text block that starts with anthropicBillingHeaderPrefix
// first in the `system` array of a Messages request. Anthropic reads the
// client version from it: a model that needs a newer Claude Code than the
// block names is refused with "Claude Code <version> does not support this
// model". A request without the block is not held to a version.
//
// The block built here follows claude-cli 2.1.329. Its form and the
// fingerprint were read from the CLI's code and checked against a request the
// CLI sent to a local server:
//
//	x-anthropic-billing-header: cc_version=<version>.<fingerprint>; cc_entrypoint=<entrypoint>; cch=<5 hex digits>;
//
// The CLI can append more fields (cc_prompt_id, cc_turn_origin and others). It
// leaves each one out itself when it has no value for it, and none is sent
// here.
const (
	anthropicBillingHeaderPrefix = "x-anthropic-billing-header:"
	// claudeCodeEntrypoint is the cc_entrypoint of an interactive CLI session,
	// the entrypoint the user agent names too.
	claudeCodeEntrypoint = "cli"
	// claudeCodeFingerprintSalt is the constant the CLI hashes into the
	// fingerprint.
	claudeCodeFingerprintSalt = "59cf53e54c78"
	// anthropicBillingHeaderAttestation is sent as cch. The CLI writes the
	// placeholder 00000 there, and its native layer puts a value computed over
	// the request in its place; how that value is computed is not known here.
	// On 2026-11-27 Anthropic answered a request the same way with this fixed
	// value, with the placeholder and without the field, so the value this app
	// has always sent stays.
	anthropicBillingHeaderAttestation = "37703"
)

// anthropicBillingHeader returns the text of the billing header block for a
// request whose fingerprint is taken from firstUserText.
func anthropicBillingHeader(firstUserText string) string {
	return anthropicBillingHeaderPrefix +
		" cc_version=" + ClaudeCodeVersion + "." + claudeCodeFingerprint(firstUserText, ClaudeCodeVersion) +
		"; cc_entrypoint=" + claudeCodeEntrypoint +
		"; cch=" + anthropicBillingHeaderAttestation + ";"
}

// claudeCodeFingerprint returns the three hex digits the CLI appends to its
// version in cc_version: the start of the SHA-256 of the salt, the characters
// at positions 4, 7 and 20 of the first user message's text, and the version.
// A position past the end of the text counts as the character "0".
//
// The CLI counts positions in UTF-16 code units, as JavaScript strings do. A
// character outside the Basic Multilingual Plane takes two units, and one unit
// of such a pair on its own is hashed as U+FFFD.
func claudeCodeFingerprint(firstUserText string, version string) string {
	positions := [...]int{4, 7, 20}
	picked := [len(positions)]string{"0", "0", "0"}
	unit := 0
	for _, char := range firstUserText {
		if unit > positions[len(positions)-1] {
			break
		}
		// Bytes that are not UTF-8 decode to U+FFFD, which is one unit.
		width, text := 1, string(char)
		if char > 0xFFFF {
			width, text = 2, "\uFFFD"
		}
		for index, position := range positions {
			if position >= unit && position < unit+width {
				picked[index] = text
			}
		}
		unit += width
	}
	sum := sha256.Sum256([]byte(claudeCodeFingerprintSalt + picked[0] + picked[1] + picked[2] + version))
	return hex.EncodeToString(sum[:])[:3]
}

// anthropicBillingFingerprintText returns the text the fingerprint of a
// request is taken from: the text of the first user message when the request
// goes to Anthropic's own API, and "" for every other host.
//
// The CLI changes the billing header block with every request (it carries a
// prompt id) and still marks the system prompt after it as cached for all
// users, so Anthropic evidently does not count the block into a cached prefix,
// and it may differ from one conversation to the next there. Another provider
// sees an ordinary system block. If that block followed each conversation's
// first message, a new conversation would find nothing of the prefix cached.
func anthropicBillingFingerprintText(requestURL string, messages []anthropicMessage) string {
	if modelchannel.URLHostForProtocol(requestURL) != "api.anthropic.com" {
		return ""
	}
	return anthropicFirstUserText(messages)
}

// anthropicFirstUserText returns the text of the first user message: its first
// text block. It is "" when there is no user message or that message has no
// text.
func anthropicFirstUserText(messages []anthropicMessage) string {
	for _, message := range messages {
		if message.Role != "user" {
			continue
		}
		for _, block := range message.Content {
			if kind, _ := block["type"].(string); kind != "text" {
				continue
			}
			text, _ := block["text"].(string)
			return text
		}
		return ""
	}
	return ""
}
