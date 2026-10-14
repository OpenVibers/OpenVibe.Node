// Package profilesig checks a community robot profile against the signature OpenVibe.Bot made when an OpenVibe admin
// approved it (Bot server/submissions.js): Ed25519 over the profile's canonical JSON — every object's keys sorted, no
// whitespace, strings and numbers exactly as JavaScript's JSON.stringify writes them — with the key Bot publishes at
// GET /api/v1/profiles/signing-key. A profile that does not check out is not the one that was reviewed: its driver
// should not be installed.
package profilesig

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf16"
)

// CommunityRe is the id of a community profile (Bot's COMMUNITY_ID): only these are submitted, reviewed and signed.
var CommunityRe = regexp.MustCompile(`^community\.[a-z][a-z0-9_-]{1,40}$`)

// Signature is the `signature` of Bot's GET /api/v1/profiles/:id.
type Signature struct {
	Alg   string `json:"alg"`
	KeyID string `json:"key_id"`
	Value string `json:"value"`
}

// Key is the `key` of Bot's GET /api/v1/profiles/signing-key.
type Key struct {
	Alg          string `json:"alg"`
	KeyID        string `json:"key_id"`
	PublicKeyPEM string `json:"public_key_pem"`
}

// ErrUnsigned: Bot serves the profile without a signature (not approved, or approved before signing existed).
var ErrUnsigned = errors.New("Bot has no signature for this profile")

// ErrBadSignature: the signature does not match the profile, so the profile changed after it was reviewed.
var ErrBadSignature = errors.New("the signature does not match the profile: it is not the profile that was reviewed")

// Canonical returns the bytes Bot signs for the JSON value raw: keys sorted by UTF-16 code units (JavaScript's
// sort), no whitespace, numbers as written (Bot writes them with JSON.stringify), strings escaped as JSON.stringify
// escapes them.
func Canonical(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("more than one JSON value")
	}
	var b bytes.Buffer
	write(&b, v)
	return b.Bytes(), nil
}

func write(b *bytes.Buffer, v any) {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case json.Number:
		b.WriteString(x.String())
	case string:
		writeString(b, x)
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			write(b, e)
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return lessUTF16(keys[i], keys[j]) })
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeString(b, k)
			b.WriteByte(':')
			write(b, x[k])
		}
		b.WriteByte('}')
	}
}

// writeString writes s as JSON.stringify does: `"` and `\` escaped, \b \f \n \r \t short, other control characters
// as \u00xx, everything else (non-ASCII, U+2028 and U+2029 included) as it is.
func writeString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

func lessUTF16(a, b string) bool {
	ua, ub := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(ua) && i < len(ub); i++ {
		if ua[i] != ub[i] {
			return ua[i] < ub[i]
		}
	}
	return len(ua) < len(ub)
}

// KeyID is Bot's id of a public key: the first 16 hex digits of the SHA-256 of its SPKI DER.
func KeyID(spkiDER []byte) string {
	sum := sha256.Sum256(spkiDER)
	return hex.EncodeToString(sum[:])[:16]
}

// Verify checks sig over profile with key. The key's id must be the one its PEM hashes to, and the one sig names.
func Verify(profile []byte, sig Signature, key Key) error {
	if sig.Alg != "Ed25519" || key.Alg != "Ed25519" {
		return fmt.Errorf("unknown signature algorithm %q (key %q)", sig.Alg, key.Alg)
	}
	block, _ := pem.Decode([]byte(key.PublicKeyPEM))
	if block == nil {
		return errors.New("Bot's signing key is not a PEM public key")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return fmt.Errorf("Bot's signing key: %w", err)
	}
	edPub, ok := pub.(ed25519.PublicKey)
	if !ok {
		return errors.New("Bot's signing key is not an Ed25519 key")
	}
	if id := KeyID(block.Bytes); id != key.KeyID {
		return fmt.Errorf("Bot's signing key hashes to %s, not the %s it is published as", id, key.KeyID)
	}
	if sig.KeyID != key.KeyID {
		return fmt.Errorf("the profile was signed with key %s; Bot publishes key %s", sig.KeyID, key.KeyID)
	}
	raw, err := base64.StdEncoding.DecodeString(sig.Value)
	if err != nil {
		return fmt.Errorf("the signature is not base64: %w", err)
	}
	canon, err := Canonical(profile)
	if err != nil {
		return fmt.Errorf("the profile is not JSON: %w", err)
	}
	if !ed25519.Verify(edPub, canon, raw) {
		return ErrBadSignature
	}
	return nil
}

// Result is what Check found out about a community profile.
type Result struct {
	ID      string
	Version string
	KeyID   string
	Plugin  string // mapping.plugin: the Node plugin the profile's robots run, installed by the owner
}

// Check fetches the community profile id and Bot's signing key from server (Bot's origin) and verifies the profile.
// With want (a profile Bot sent this Node, at pairing), it must also be that same profile. pinKeyID, when set, is the
// only key accepted.
func Check(ctx context.Context, client *http.Client, server, id string, want json.RawMessage, pinKeyID string) (Result, error) {
	res := Result{ID: id}
	if !CommunityRe.MatchString(id) {
		return res, fmt.Errorf("%q is not a community profile id (community.<name>): only those are reviewed and signed", id)
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	base := strings.TrimSuffix(server, "/") + "/api/v1/profiles/"
	var doc struct {
		Profile   json.RawMessage `json:"profile"`
		Signature *Signature      `json:"signature"`
	}
	if err := getJSON(ctx, client, base+url.PathEscape(id), &doc); err != nil {
		return res, err
	}
	var meta struct {
		ID      string         `json:"id"`
		Version any            `json:"version"`
		Mapping map[string]any `json:"mapping"`
	}
	_ = json.Unmarshal(doc.Profile, &meta)
	if meta.Version != nil {
		res.Version = fmt.Sprint(meta.Version)
	}
	if p, ok := meta.Mapping["plugin"].(string); ok {
		res.Plugin = p
	}
	if meta.ID != id {
		return res, fmt.Errorf("Bot answered with profile %q for %q", meta.ID, id)
	}
	if doc.Signature == nil {
		return res, ErrUnsigned
	}
	var keyDoc struct {
		Key Key `json:"key"`
	}
	if err := getJSON(ctx, client, base+"signing-key", &keyDoc); err != nil {
		return res, fmt.Errorf("Bot's signing key: %w", err)
	}
	res.KeyID = keyDoc.Key.KeyID
	if pinKeyID != "" && keyDoc.Key.KeyID != pinKeyID {
		return res, fmt.Errorf("Bot signs with key %s, not the pinned %s", keyDoc.Key.KeyID, pinKeyID)
	}
	if err := Verify(doc.Profile, *doc.Signature, keyDoc.Key); err != nil {
		return res, err
	}
	if len(want) > 0 {
		a, errA := Canonical(want)
		b, errB := Canonical(doc.Profile)
		if errA != nil || errB != nil || !bytes.Equal(a, b) {
			return res, errors.New("the profile Bot sent this Node is not the signed one in its catalogue (another version, or changed)")
		}
	}
	return res, nil
}

func getJSON(ctx context.Context, client *http.Client, u string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%s: not found", u)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", u, resp.StatusCode)
	}
	return json.Unmarshal(body, into)
}
