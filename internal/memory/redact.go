package memory

import (
	"regexp"
	"strings"
)

// secretPattern is one shape of secret and how to replace it. Keep is the
// number of capture groups that are kept in front of the redacted part, so an
// assignment still reads as "password=[redacted]".
type secretPattern struct {
	re   *regexp.Regexp
	Keep int
	// Template, when set, is used to rebuild the match: "$1" and friends are
	// the capture groups, and the redacted marker replaces the secret.
	Template string
}

// secretPatterns are the shapes we remove before an item is shown or stored.
//
// The first five are the patterns of OpenVibe.Codes' harness (harness/events.js,
// SECRET_PATTERNS); the rest are the common gitleaks shapes: AWS keys, GitHub
// tokens, Slack tokens, Stripe keys, OpenAI- and Anthropic-style keys, JWTs,
// PEM private keys, password/token/secret assignments and connection strings
// that carry a password.
var secretPatterns = []secretPattern{
	{re: regexp.MustCompile("\\b(?:sk|pk|rk)-[A-Za-z0-9_-]{16,}")},
	{re: regexp.MustCompile("\\bgh[pousr]_[A-Za-z0-9]{20,}")},
	{re: regexp.MustCompile("\\bgithub_pat_[A-Za-z0-9_]{20,}")},
	{re: regexp.MustCompile("\\beyJ[A-Za-z0-9_-]{8,}\\.[A-Za-z0-9_-]{8,}\\.[A-Za-z0-9_-]{8,}")},
	{re: regexp.MustCompile("(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----")},
	{re: regexp.MustCompile("\\bAKIA[0-9A-Z]{16}")},
	{re: regexp.MustCompile("\\bASIA[0-9A-Z]{16}")},
	{re: regexp.MustCompile("\\bxox[baprs]-[A-Za-z0-9-]{10,}")},
	{re: regexp.MustCompile("\\b(?:sk|rk|pk)_(?:live|test)_[A-Za-z0-9]{10,}")},
	// An assignment: the key and the separator are kept, the value is not.
	{
		re:   regexp.MustCompile("(?i)\\b(api[_-]?key|access[_-]?token|client[_-]?secret|token|password|secret)\\b([ \\t]*[:=][ \\t]*[\"']?)([^\\s\"',;]{6,})"),
		Keep: 2,
	},
	{
		re:   regexp.MustCompile("(?i)(aws_secret_access_key)([ \\t]*[:=][ \\t]*[\"']?)([A-Za-z0-9/+=]{40,})[\"']?"),
		Keep: 2,
	},
	// A connection string with a password: the scheme, the user and the host
	// are kept, the password is not.
	{
		re:       regexp.MustCompile("([a-zA-Z][a-zA-Z0-9+.-]*://[^\\s:/@]+:)([^\\s@/]+)(@[^\\s\"']*)"),
		Keep:     1,
		Template: "${1}" + Redacted + "${3}",
	},
}

// Redacted is what a secret is replaced with.
const Redacted = "[redacted]"

// Redact removes the secrets from a text. It returns the text and how many
// secrets were removed; the secrets themselves are never returned.
func Redact(text string) (string, int) {
	count := 0
	for _, p := range secretPatterns {
		text = p.replace(text, &count)
	}
	return text, count
}

// replace swaps every match of the pattern for the redacted marker, keeping
// the groups the pattern says to keep.
func (p secretPattern) replace(text string, count *int) string {
	return p.re.ReplaceAllStringFunc(text, func(m string) string {
		*count++
		if p.Template != "" {
			return p.re.ReplaceAllString(m, p.Template)
		}
		if p.Keep == 0 {
			return Redacted
		}
		groups := p.re.FindStringSubmatch(m)
		if len(groups) <= p.Keep {
			return Redacted
		}
		kept := ""
		for i := 1; i <= p.Keep; i++ {
			kept += groups[i]
		}
		return kept + Redacted
	})
}

// onlySecret tells whether a text holds nothing but redacted secrets.
func onlySecret(text string) bool {
	rest := strings.ReplaceAll(text, Redacted, "")
	return strings.TrimSpace(rest) == ""
}
