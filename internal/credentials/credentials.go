// Package credentials stores the device credential and publish key the pairing returned. The file is mode 0600 and
// the secret values are wrapped in Secret, whose every string form is "[redacted]", so they cannot reach a log line,
// an error or `openvibe-node status` by accident.
//
// Two kinds of pairing share the file. Legacy (OpenVibe.Bot's POST /api/v1/pair): a Bot device credential. Network
// (OpenVibe.Network's POST /api/v1/node-pairing): a node principal and its node credential, which buys node tokens;
// the device fields (device id, publish key, WHIP URL, robot, profile) come from Bot's POST /api/v1/devices/bind and
// are empty until that bind has succeeded once.
package credentials

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
)

const redacted = "[redacted]"

// Secret holds a value that must never be printed. Use Reveal only where the value goes on the wire.
type Secret struct{ v string }

func NewSecret(s string) Secret { return Secret{v: s} }

// Reveal returns the secret. Call it only to put the value in an Authorization header or the credential file.
func (s Secret) Reveal() string { return s.v }

func (s Secret) IsZero() bool { return s.v == "" }

func (s Secret) String() string   { return redacted }
func (s Secret) GoString() string { return redacted }

// Format makes %v, %+v, %#v, %s and %q all print "[redacted]".
func (s Secret) Format(f fmt.State, verb rune) { _, _ = f.Write([]byte(redacted)) }

func (s Secret) MarshalJSON() ([]byte, error) { return json.Marshal(redacted) }

// UnmarshalJSON refuses: secrets are read only through Load.
func (s *Secret) UnmarshalJSON([]byte) error {
	return errors.New("credentials: secrets are not decoded from JSON")
}

// Credentials is what `openvibe-node pair` stores.
type Credentials struct {
	DeviceID   string
	RobotID    string // rob_… the device was paired to
	Credential Secret // a Bot device credential (legacy pairing); zero for a Network-paired machine
	// Network pairing: the node principal (nod_…), its credential, the Network origin and what it was paired for.
	Principal      string
	NodeCredential Secret
	Network        string
	PairedFor      *protocol.PairedFor
	PublishKey     Secret // WHIP publish key; secret like the credential
	Server         string
	DeviceURL      string
	WHIPURL        string
	ICEServers     []protocol.ICEServer
	Profile        json.RawMessage
	PairedAt       time.Time
}

// file is the on-disk form; only Save and Load see plain secrets.
type file struct {
	Version        int                 `json:"version"`
	Principal      string              `json:"principal,omitempty"`
	NodeCredential string              `json:"node_credential,omitempty"`
	Network        string              `json:"network,omitempty"`
	PairedFor      *protocol.PairedFor `json:"paired_for,omitempty"`

	DeviceID   string               `json:"device_id,omitempty"`
	RobotID    string               `json:"robot_id,omitempty"`
	Credential string               `json:"credential,omitempty"`
	PublishKey string               `json:"publish_key"`
	Server     string               `json:"server"`
	DeviceURL  string               `json:"device_url,omitempty"`
	WHIPURL    string               `json:"whip_url,omitempty"`
	ICEServers []protocol.ICEServer `json:"ice_servers,omitempty"`
	Profile    json.RawMessage      `json:"profile,omitempty"`
	PairedAt   time.Time            `json:"paired_at"`
}

// NetworkPaired reports a machine paired through OpenVibe.Network: it authenticates with node tokens.
func (c *Credentials) NetworkPaired() bool { return c != nil && c.Principal != "" }

// Bound reports whether the device fields are present: always for a legacy pairing, after a successful
// POST /api/v1/devices/bind for a Network one.
func (c *Credentials) Bound() bool { return c != nil && c.DeviceID != "" }

// Label names the pairing for messages: the device id, or the node principal before it is bound.
func (c *Credentials) Label() string {
	if c.DeviceID != "" {
		return c.DeviceID
	}
	return c.Principal
}

// valid is the shape Save and Load accept: a device id and Bot credential, or a node principal, its credential and
// the Network origin.
func valid(principal, nodeCredential, network, deviceID, credential string) bool {
	if principal != "" {
		return nodeCredential != "" && network != ""
	}
	return deviceID != "" && credential != ""
}

// ErrNotPaired is returned by Load when there is no credential file.
var ErrNotPaired = errors.New("this device is not paired; run `openvibe-node pair <CODE>`")

// Save writes the credentials atomically with mode 0600.
func Save(path string, c *Credentials) error {
	if !valid(c.Principal, c.NodeCredential.Reveal(), c.Network, c.DeviceID, c.Credential.Reveal()) {
		return errors.New("credentials: refusing to save an empty credential")
	}
	b, err := json.MarshalIndent(file{
		Version: 1, Principal: c.Principal, NodeCredential: c.NodeCredential.Reveal(), Network: c.Network, PairedFor: c.PairedFor,
		DeviceID: c.DeviceID, RobotID: c.RobotID, Credential: c.Credential.Reveal(), PublishKey: c.PublishKey.Reveal(),
		Server: c.Server, DeviceURL: c.DeviceURL, WHIPURL: c.WHIPURL, ICEServers: c.ICEServers, Profile: c.Profile,
		PairedAt: c.PairedAt,
	}, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("credentials: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".credential-*")
	if err != nil {
		return fmt.Errorf("credentials: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		tmp.Close()
		return fmt.Errorf("credentials: %w", err)
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("credentials: write failed")
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("credentials: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("credentials: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("credentials: %w", err)
	}
	return os.Chmod(path, 0o600)
}

// Load reads the credentials. A file readable by group or others is tightened to 0600 and reported through warn.
func Load(path string, warn func(string)) (*Credentials, error) {
	st, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotPaired
	}
	if err != nil {
		return nil, fmt.Errorf("credentials: %w", err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, fmt.Errorf("credentials: %s is readable by others (mode %o) and could not be fixed: %w", path, st.Mode().Perm(), err)
		}
		if warn != nil {
			warn(fmt.Sprintf("credential file %s was mode %o; set it to 600", path, st.Mode().Perm()))
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("credentials: %w", err)
	}
	var f file
	if err := json.Unmarshal(b, &f); err != nil {
		// Never wrap the decoder error: it can quote file contents.
		return nil, fmt.Errorf("credentials: %s is not valid JSON", path)
	}
	if !valid(f.Principal, f.NodeCredential, f.Network, f.DeviceID, f.Credential) {
		return nil, fmt.Errorf("credentials: %s is incomplete; pair again", path)
	}
	return &Credentials{
		Principal: f.Principal, NodeCredential: NewSecret(f.NodeCredential), Network: f.Network, PairedFor: f.PairedFor,
		DeviceID: f.DeviceID, RobotID: f.RobotID, Credential: NewSecret(f.Credential), PublishKey: NewSecret(f.PublishKey),
		Server: f.Server, DeviceURL: f.DeviceURL, WHIPURL: f.WHIPURL, ICEServers: f.ICEServers, Profile: f.Profile,
		PairedAt: f.PairedAt,
	}, nil
}
