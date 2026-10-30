package memory

import (
	"fmt"
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
	// the capture groups, and "%s" is the marker that replaces the secret.
	Template string
}

// secretNameSuffixes are the words a credential name may end in. A name is a
// run of [A-Za-z0-9_.-] that ends in one of these, so DATABASE_PASSWORD,
// MY_SECRET and GITHUB_TOKEN are names, and so are password and api_key.
const secretNameSuffixes = "password|passwd|pwd|passphrase|secret|token|api[_-]?key|apikey|access[_-]?key|private[_-]?key|client[_-]?secret|auth|credential|credentials|cookie|session"

// secretNamePattern is a credential name: a run of [A-Za-z0-9_.-] that ends in
// one of the words above. The name may end in an "s" as well, so that
// "max_tokens" and "api_keys" are names too.
const secretNamePattern = "(?:" + secretNameSuffixes + ")s?"

// secretPatterns are the shapes we remove before an item is shown or stored.
//
// The first five are the patterns of OpenVibe.Codes' harness (harness/events.js,
// SECRET_PATTERNS). The rest are the common gitleaks shapes and the shapes
// that turn up in a person's own files: AWS keys, GitHub and GitLab tokens,
// Slack and Discord tokens, Stripe keys, OpenAI- and Anthropic-style keys,
// JWTs, PEM private keys in every form, env-style assignments, headers, the
// webhooks of chat services and the card numbers that pass the Luhn check.
var secretPatterns = []secretPattern{
	// A webhook URL carries its token in the path: the whole URL goes.
	{re: regexp.MustCompile("https://hooks\\.slack\\.com/services/[^\\s\"']+")},
	{re: regexp.MustCompile("https://(?:discord|discordapp)\\.com/api/webhooks/[^\\s\"']+")},
	// A connection string with a password: the scheme, the user and the host
	// stay, the password goes.
	{
		re:       regexp.MustCompile("([a-zA-Z][a-zA-Z0-9+.-]*://[^\\s:/@]+:)([^\\s@/]+)(@[^\\s\"']*)"),
		Keep:     1,
		Template: "$1%s$3",
	},
	// A private key block, in every form: RSA, EC, DSA, OPENSSH, PGP and
	// ENCRYPTED.
	{re: regexp.MustCompile("(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY[A-Z ]*-----.*?-----END [A-Z0-9 ]*PRIVATE KEY[A-Z ]*-----")},
	// The keys of the tools and the services.
	{re: regexp.MustCompile("\\b(?:sk|pk|rk)-[A-Za-z0-9_-]{16,}")},
	{re: regexp.MustCompile("\\bgh[pousr]_[A-Za-z0-9]{20,}")},
	{re: regexp.MustCompile("\\bgithub_pat_[A-Za-z0-9_]{20,}")},
	{re: regexp.MustCompile("\\bglpat-[0-9A-Za-z_-]{20,}")},
	{re: regexp.MustCompile("\\bnpm_[A-Za-z0-9]{36}")},
	{re: regexp.MustCompile("\\bhf_[A-Za-z0-9]{30,}")},
	{re: regexp.MustCompile("\\bAIza[0-9A-Za-z_-]{35}")},
	{re: regexp.MustCompile("\\bSK[0-9a-fA-F]{32}\\b")},
	{re: regexp.MustCompile("\\bxox[baprs]-[A-Za-z0-9-]{10,}")},
	{re: regexp.MustCompile("\\bxapp-[0-9A-Za-z-]{10,}")},
	{re: regexp.MustCompile("\\b(?:sk|rk|pk)_(?:live|test)_[A-Za-z0-9]{10,}")},
	{re: regexp.MustCompile("\\bSG\\.[A-Za-z0-9_-]{16,}\\.[A-Za-z0-9_-]{16,}")},
	{re: regexp.MustCompile("\\b\\d{8,10}:[A-Za-z0-9_-]{35}\\b")},
	{re: regexp.MustCompile("[MN][A-Za-z\\d]{23,25}\\.[\\w-]{6}\\.[\\w-]{27,}")},
	{re: regexp.MustCompile("\\bhbt_[A-Za-z0-9_-]{16,}")},
	{re: regexp.MustCompile("\\bpgsk_[A-Za-z0-9_]{16,}")},
	{re: regexp.MustCompile("\\bAKIA[0-9A-Z]{16}")},
	{re: regexp.MustCompile("\\bASIA[0-9A-Z]{16}")},
	{re: regexp.MustCompile("\\beyJ[A-Za-z0-9_-]{8,}\\.[A-Za-z0-9_-]{8,}\\.[A-Za-z0-9_-]{8,}")},
	// A header that carries a credential.
	{
		re:   regexp.MustCompile("(?i)(authorization[ \\t]*:[ \\t]*(?:bearer|basic)[ \\t]+)(\\S+)"),
		Keep: 1,
	},
	{
		re:   regexp.MustCompile("(?i)(x-api-key[ \\t]*:[ \\t]*)(\\S+)"),
		Keep: 1,
	},
	{
		re:   regexp.MustCompile("(?i)\\b(bearer[ \\t]+)([A-Za-z0-9._~+/=-]{20,})"),
		Keep: 1,
	},
	// An assignment: the name and the separator stay, the value goes. The
	// name ends in a word that means "this is a credential".
	//
	// A quoted value keeps its quotes, so the text still reads as the
	// config it was.
	{
		re:       regexp.MustCompile("(?i)([A-Za-z0-9_.-]*" + secretNamePattern + ")([\"']?[ \\t]*(?:=>|[:=])[ \\t]*)\"([^\"\\n]{4,})\""),
		Template: "$1$2\"%s\"",
	},
	{
		re:       regexp.MustCompile("(?i)([A-Za-z0-9_.-]*" + secretNamePattern + ")([\"']?[ \\t]*(?:=>|[:=])[ \\t]*)'([^'\\n]{4,})'"),
		Template: "$1$2'%s'",
	},
	// A value whose closing quote was lost: it still goes, to the end of
	// the line.
	{
		re:       regexp.MustCompile("(?i)([A-Za-z0-9_.-]*" + secretNamePattern + ")([\"']?[ \\t]*(?:=>|[:=])[ \\t]*)\"([^\"\\n]{4,})"),
		Template: "$1$2\"%s",
	},
	// An unquoted value.
	{
		re:   regexp.MustCompile("(?i)([A-Za-z0-9_.-]*" + secretNamePattern + ")([\"']?[ \\t]*(?:=>|[:=])[ \\t]*)([^\\s\"',;]{6,})"),
		Keep: 2,
	},
	// Prose: "the password is hunter2".
	{
		re:   regexp.MustCompile("(?i)\\b(password|passphrase|passcode)([ \\t]+(?:is|was|=)[ \\t]+)(\\S{4,})"),
		Keep: 2,
	},
}

// cardCandidate is a run of 13 to 19 digits with optional spaces or dashes
// between them. Whether it is a card is decided by the Luhn check.
var cardCandidate = regexp.MustCompile("[0-9](?:[ -]?[0-9]){12,18}")

// pemBegin and pemEnd are the lines of a private key block.
var (
	pemBegin = regexp.MustCompile("-----BEGIN [A-Z0-9 ]*PRIVATE KEY[A-Z ]*-----")
	pemEnd   = regexp.MustCompile("-----END [A-Z0-9 ]*PRIVATE KEY[A-Z ]*-----")
)

// The bytes of the two fence markers of markdown.
const (
	fenceTick  = 0x60
	fenceTilde = 0x7e
)

// Redacted is what a secret is replaced with.
const Redacted = "[redacted]"

// secretMark is the sentinel the body of a file is scrubbed with, before it
// is split into items. It is a string that never appears in a real file, so
// that an item can still count the secrets the whole body held.
const secretMark = "\x00redacted\x00"

// Redact removes the secrets from a text. It returns the text and how many
// secrets were removed; the secrets themselves are never returned.
//
// It is safe to call on a text that was already redacted: a value that is
// already the redacted marker is left alone.
func Redact(text string) (string, int) {
	return redactWith(text, Redacted)
}

// redactWith removes the secrets from a text and writes the marker in place
// of each one.
//
// The marker is a parameter so that a caller that scrubs a whole file before
// splitting it into items can still tell how many secrets each item held: it
// counts the markers it finds.
func redactWith(text string, marker string) (string, int) {
	count := 0
	text = redactUnterminatedKey(text, marker, &count)
	for _, p := range secretPatterns {
		text = p.replace(text, marker, &count)
	}
	text = redactCards(text, marker, &count)
	return text, count
}

// replace swaps every match of the pattern for the redacted marker, keeping
// the groups the pattern says to keep.
func (p secretPattern) replace(text string, marker string, count *int) string {
	return p.re.ReplaceAllStringFunc(text, func(m string) string {
		if strings.Contains(m, marker) {
			// Already redacted: there is nothing to hide here.
			return m
		}
		*count++
		if p.Template != "" {
			return p.re.ReplaceAllString(m, fmt.Sprintf(p.Template, marker))
		}
		if p.Keep == 0 {
			return marker
		}
		groups := p.re.FindStringSubmatch(m)
		if len(groups) <= p.Keep {
			return marker
		}
		kept := ""
		for i := 1; i <= p.Keep; i++ {
			kept += groups[i]
		}
		return kept + marker
	})
}

// redactUnterminatedKey redacts a private key block that has no END line: a
// truncated file, or a key pasted in halves.
//
// The key is redacted from its BEGIN line to the end of the fence it sits in,
// or to the end of the text when it is not in a fence. A block that has its
// END line is left to the pattern list, which redacts the whole block.
func redactUnterminatedKey(text string, marker string, count *int) string {
	blocks := pemBegin.FindAllStringIndex(text, -1)
	// Work from the last block backwards, so the indexes of the earlier ones
	// stay valid.
	for i := len(blocks) - 1; i >= 0; i-- {
		start, after := blocks[i][0], blocks[i][1]
		if pemEnd.MatchString(text[after:]) {
			// The block is complete: the pattern list redacts it whole.
			continue
		}
		end := len(text)
		if fence := fenceEnd(text, start); fence > start {
			end = fence
		}
		text = text[:start] + marker + "\n" + text[end:]
		*count++
	}
	return text
}

// fenceEnd returns the index of the fence that closes the block the given
// position sits in. It returns the end of the text when the position is not
// inside a fence.
func fenceEnd(text string, pos int) int {
	char, width := openFence(text, pos)
	if char == 0 {
		// Not inside a fence: the key runs to the end of the text.
		return len(text)
	}
	for i := pos; i < len(text); {
		n := fenceRun(text, i)
		if n == 0 {
			i++
			continue
		}
		if text[i] == char && n >= width {
			return i
		}
		i += n
	}
	return len(text)
}

// openFence returns the character and the width of the fence that is open at
// the given position, or 0 when the position is not inside a fence.
func openFence(text string, pos int) (byte, int) {
	var char byte
	var width int
	for i := 0; i < pos; {
		n := fenceRun(text, i)
		if n == 0 {
			i++
			continue
		}
		if char == 0 {
			char, width = text[i], n
		} else if text[i] == char && n >= width {
			char, width = 0, 0
		}
		i += n
	}
	return char, width
}

// fenceRun returns the length of the run of fence characters at s[i] when the
// run is at least three characters long, and 0 otherwise.
func fenceRun(s string, i int) int {
	if i >= len(s) || (s[i] != fenceTick && s[i] != fenceTilde) {
		return 0
	}
	n := 0
	for i+n < len(s) && s[i+n] == s[i] {
		n++
	}
	if n < 3 {
		return 0
	}
	return n
}

// redactCards redacts the card numbers that pass the Luhn check. A long
// number that does not pass it is left alone: an order number is not a card.
func redactCards(text string, marker string, count *int) string {
	var out strings.Builder
	last := 0
	for _, loc := range cardCandidate.FindAllStringIndex(text, -1) {
		start, end := loc[0], loc[1]
		if start > 0 && isDigit(text[start-1]) {
			// Part of a longer number.
			continue
		}
		j := end
		for j < len(text) && text[j] == ' ' {
			j++
		}
		if j < len(text) && isDigit(text[j]) {
			// The number continues past the match.
			continue
		}
		digits := ""
		for i := start; i < end; i++ {
			if isDigit(text[i]) {
				digits += string(text[i])
			}
		}
		if len(digits) < 13 || len(digits) > 19 || !luhn(digits) {
			continue
		}
		out.WriteString(text[last:start])
		out.WriteString(marker)
		*count++
		last = end
	}
	out.WriteString(text[last:])
	return out.String()
}

// isDigit tells whether a byte is a digit.
func isDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

// luhn tells whether a run of digits is a card number.
func luhn(digits string) bool {
	sum := 0
	double := false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}

// onlySecret tells whether a text holds nothing but redacted secrets.
func onlySecret(text string) bool {
	rest := strings.ReplaceAll(text, Redacted, "")
	return strings.TrimSpace(rest) == ""
}
