package tailscale

import (
	"regexp"
	"strings"
)

const redacted = "[redacted]"

var redactions = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	// Interactive login and machine authorization URLs: https://<host>/a/<token>.
	{regexp.MustCompile(`(?i)(https?://[^\s/"'<>]+)/a/[^\s"'<>]+`), "${1}/a/" + redacted},
	// Auth keys, OAuth client secrets and API keys.
	{regexp.MustCompile(`tskey-[A-Za-z0-9_-]+(?:-[A-Za-z0-9_]+)*`), "tskey-" + redacted},
	// Node, machine, disco and network-lock keys.
	{regexp.MustCompile(`\b(nodekey|mkey|privkey|discokey|nlpub|nlpriv|tlpub):[0-9a-fA-F]{16,}`), "${1}:" + redacted},
	{regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`), "${1} " + redacted},
	{regexp.MustCompile(`(?i)\b((?:set-)?cookie\s*[:=]\s*)[^\r\n]+`), "${1}" + redacted},
	{regexp.MustCompile(`(?i)\b(auth[-_]?key|authkey|secret|password)(["']?\s*[:=]\s*["']?)[^\s"',}]+`), "${1}${2}" + redacted},
}

// Redact removes Tailscale login URLs, keys and credentials from text that may
// reach logs, API errors or diagnostics.
func Redact(text string) string {
	if text == "" {
		return text
	}
	for _, redaction := range redactions {
		text = redaction.pattern.ReplaceAllString(text, redaction.replacement)
	}
	return text
}

// RedactLog formats a tsnet log line. tsnet prints the interactive login URL
// through its user logger; it must never reach Mihomo log files or streams.
func RedactLog(line string) string {
	line = Redact(line)
	if strings.Contains(line, "/a/"+redacted) {
		return line + " (open the login flow from the app)"
	}
	return line
}
