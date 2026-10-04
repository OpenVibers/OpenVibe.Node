//go:build linux

package worker

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/OpenVibers/OpenVibe.Node/internal/config"
)

// A job under worker.egress public or openvibe-only (docs/worker.md, "Egress") gets, in its network namespace, one end
// of a veth pair whose other end, ovw<n>, stays in the host's; the two ends hold a /30 of vethPool and the job's
// default route goes through the host's end. The nftables table openvibe_worker, in the host's namespace, masquerades
// what a job sends and refuses (a TCP reset, otherwise ICMP administratively prohibited) everything else: anything to
// the host itself, IPv6, a route other than the host's default one, the nonPublic ranges, worker.egress_deny and, for
// openvibe-only, whatever lies outside worker.egress_allow. Nothing new reaches a job, only replies. The table is
// written whole, in one transaction, each time a job's veth is made, so a firewall reload that dropped it is undone by
// the next job. A job naming net "deny", or under egress none, gets no veth: its loopback, down, is all it has.

const (
	egressTable = "openvibe_worker"
	vethPrefix  = "ovw"
	vethPool    = "169.254.240.0/20" // link-local: an address a job is refused, so a job reaches no other's veth
	vethMax     = 1024               // the /30s in vethPool
)

// nonPublic is the IPv4 ranges a job under public or openvibe-only is always refused: this network, private (RFC
// 1918), shared (100.64/10: carrier NAT and most WireGuard meshes), loopback, link-local (cloud metadata included),
// IETF protocol assignments, documentation, the 6to4 relay, benchmarking, multicast and reserved.
var nonPublic = []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
	"192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24",
	"203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4"}

// egressToolDirs is where the Node finds ip, nsenter and nft, which it runs as root (not from $PATH); tests change it.
var egressToolDirs = []string{"/usr/sbin", "/sbin", "/usr/bin", "/bin"}

// errEgressTool: a program the egress policies need is not installed.
var errEgressTool = errors.New("not installed")

var (
	vethMu   sync.Mutex
	vethUsed = map[int]bool{}
	vethNext int // round robin: a pair just deleted is not made again at once
)

type egressTools struct{ ip, nsenter, nft string }

// egressReady refuses an egress policy the host cannot enforce: the Node must run as root, find ip, nsenter and nft,
// and the host must forward IPv4 (net.ipv4.ip_forward=1, which the Node does not set: it is the owner's choice).
func egressReady() (egressTools, error) {
	var t egressTools
	if os.Geteuid() != 0 {
		return t, errors.New("needs the Node to run as root (worker.run_as)")
	}
	for _, x := range []struct {
		name string
		path *string
	}{{"ip", &t.ip}, {"nsenter", &t.nsenter}, {"nft", &t.nft}} {
		for _, d := range egressToolDirs {
			p := filepath.Join(d, x.name)
			if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
				*x.path = p
				break
			}
		}
		if *x.path == "" {
			return t, fmt.Errorf("needs %s (%s): %w", x.name, strings.Join(egressToolDirs, ", "), errEgressTool)
		}
	}
	if b, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward"); err != nil || strings.TrimSpace(string(b)) != "1" {
		return t, errors.New("needs IPv4 forwarding on the host (sysctl net.ipv4.ip_forward=1)")
	}
	return t, nil
}

// egressNet is a job's veth pair: made before the job starts, its job end moved into the job's namespace once the
// sandbox stands.
type egressNet struct {
	tools     egressTools
	n         int  // ovw<n>; -1 once released
	made      bool // the pair exists (this Node made it)
	host, job netip.Addr
}

func (e *egressNet) hostIf() string { return vethPrefix + strconv.Itoa(e.n) }
func (e *egressNet) jobIf() string  { return e.hostIf() + "j" }

// newEgress writes the table for cfg and makes a job's veth pair, its host end addressed and up. Every error wraps
// what the host lacks, so the probe and the job fail on it.
func newEgress(cfg config.WorkerConfig) (*egressNet, error) {
	t, err := egressReady()
	if err != nil {
		return nil, err
	}
	up, err := uplinks(t.ip)
	if err != nil {
		return nil, err
	}
	if err := runTool(ruleset(cfg, up), t.nft, "-f", "-"); err != nil {
		return nil, fmt.Errorf("writing the nftables table: %w", err)
	}
	e := &egressNet{tools: t, n: -1}
	vethMu.Lock()
	for i := range vethMax {
		if n := (vethNext + i) % vethMax; !vethUsed[n] {
			e.n, vethUsed[n], vethNext = n, true, n+1
			break
		}
	}
	vethMu.Unlock()
	if e.n < 0 {
		return nil, errors.New("every job veth is in use")
	}
	b := netip.MustParsePrefix(vethPool).Addr().As4()
	base := binary.BigEndian.Uint32(b[:]) + uint32(4*e.n)
	e.host, e.job = addr4(base+1), addr4(base+2)
	if err := runTool("", t.ip, "link", "add", e.hostIf(), "type", "veth", "peer", "name", e.jobIf()); err != nil {
		e.close()
		return nil, err
	}
	e.made = true
	batch := fmt.Sprintf("addr add %s/30 dev %s\nlink set %s up\n", e.host, e.hostIf(), e.hostIf())
	if err := runTool(batch, t.ip, "-batch", "-"); err != nil {
		e.close()
		return nil, err
	}
	return e, nil
}

func addr4(v uint32) netip.Addr {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return netip.AddrFrom4(b)
}

// attach moves the job end of the pair into the network namespace of pid (the sandbox init, waiting to execute the
// function), addresses it with a default route through the host end, then reads the namespace back: the job end is
// its only interface up (the loopback stays down) and its routes are that one and its /30's.
func (e *egressNet) attach(pid int) error {
	if err := runTool("", e.tools.ip, "link", "set", e.jobIf(), "netns", strconv.Itoa(pid)); err != nil {
		return err
	}
	in := []string{"--net=/proc/" + strconv.Itoa(pid) + "/ns/net", "--", e.tools.ip}
	batch := fmt.Sprintf("addr add %s/30 dev %s\nlink set %s up\nroute add default via %s dev %s\n",
		e.job, e.jobIf(), e.jobIf(), e.host, e.jobIf())
	if err := runTool(batch, e.tools.nsenter, append(in, "-batch", "-")...); err != nil {
		return err
	}
	links, err := toolOutput(e.tools.nsenter, append(in, "-o", "link", "show", "up")...)
	if err != nil {
		return err
	}
	var up []string
	for _, l := range strings.Split(strings.TrimSpace(links), "\n") {
		if f := strings.Fields(l); len(f) > 1 {
			name, _, _ := strings.Cut(strings.TrimSuffix(f[1], ":"), "@")
			up = append(up, name)
		}
	}
	if !slices.Equal(up, []string{e.jobIf()}) {
		return fmt.Errorf("the job's interfaces up are %v, not %s alone", up, e.jobIf())
	}
	routes, err := toolOutput(e.tools.nsenter, append(in, "-4", "route", "show")...)
	if err != nil {
		return err
	}
	want := []string{"default via " + e.host.String() + " dev " + e.jobIf(),
		netip.PrefixFrom(e.job, 30).Masked().String() + " dev " + e.jobIf()}
	got := strings.Split(strings.TrimSpace(routes), "\n")
	if len(got) != len(want) || !slices.ContainsFunc(got, func(l string) bool { return routeIs(l, want[0]) }) ||
		!slices.ContainsFunc(got, func(l string) bool { return routeIs(l, want[1]) }) {
		return fmt.Errorf("the job's routes are %q, not %q", got, want)
	}
	return nil
}

func routeIs(line, route string) bool {
	line = strings.TrimSpace(line)
	return line == route || strings.HasPrefix(line, route+" ")
}

// close deletes the pair (gone already if the job's namespace is) and releases its number.
func (e *egressNet) close() {
	if e.n < 0 {
		return
	}
	if e.made {
		_ = runTool("", e.tools.ip, "link", "del", e.hostIf())
	}
	vethMu.Lock()
	delete(vethUsed, e.n)
	vethMu.Unlock()
	e.n, e.made = -1, false
}

// ifName is an interface name nft may hold in a string.
var ifName = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,15}$`)

// uplinks are the interfaces of the host's IPv4 default routes, the only ones a job's traffic may leave by (so a
// WireGuard peer, a container bridge or a LAN reached by its own route is refused even at a public address).
func uplinks(ip string) ([]string, error) {
	out, err := toolOutput(ip, "-4", "route", "show", "default")
	if err != nil {
		return nil, err
	}
	var devs []string
	f := strings.Fields(out)
	for i := 0; i+1 < len(f); i++ {
		if d := f[i+1]; f[i] == "dev" && !slices.Contains(devs, d) {
			if !ifName.MatchString(d) || strings.HasPrefix(d, vethPrefix) {
				return nil, fmt.Errorf("the default route's interface %q is not usable", d)
			}
			devs = append(devs, d)
		}
	}
	if len(devs) == 0 {
		return nil, errors.New("the host has no IPv4 default route")
	}
	return devs, nil
}

// ruleset is the nft script that replaces the table openvibe_worker for cfg, jobs leaving by uplinks.
func ruleset(cfg config.WorkerConfig, uplinks []string) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	w("table inet %s", egressTable)
	w("delete table inet %s", egressTable)
	w("table inet %s {", egressTable)
	w("\tchain refuse {")
	w("\t\tmeta l4proto tcp reject with tcp reset")
	w("\t\treject with icmpx admin-prohibited")
	w("\t}")
	w("\tchain job {")
	w("\t\tmeta nfproto != ipv4 goto refuse")
	w("\t\toifname != { \"%s\" } goto refuse", strings.Join(uplinks, "\", \""))
	for _, c := range append(slices.Clone(nonPublic), cfg.EgressDeny...) {
		w("\t\tip daddr %s goto refuse", c)
	}
	if cfg.Egress == config.EgressOpenVibeOnly {
		for _, c := range cfg.EgressAllow {
			w("\t\tip daddr %s accept", c)
		}
		w("\t\tgoto refuse")
	}
	w("\t\taccept")
	w("\t}")
	w("\tchain forward {")
	w("\t\ttype filter hook forward priority -10; policy accept;")
	w("\t\tiifname \"%s*\" goto job", vethPrefix)
	w("\t\toifname \"%s*\" ct state established,related accept", vethPrefix)
	w("\t\toifname \"%s*\" goto refuse", vethPrefix)
	w("\t}")
	w("\tchain input {")
	w("\t\ttype filter hook input priority -10; policy accept;")
	w("\t\tiifname \"%s*\" goto refuse", vethPrefix)
	w("\t}")
	w("\tchain postrouting {")
	w("\t\ttype nat hook postrouting priority 100; policy accept;")
	w("\t\tip saddr %s oifname != \"%s*\" masquerade", vethPool, vethPrefix)
	w("\t}")
	w("}")
	return b.String()
}

// runTool runs one of egressTools with stdin, failing with its output.
func runTool(stdin, path string, args ...string) error {
	cmd := exec.Command(path, args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s %s: %w: %s", filepath.Base(path), strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
}

func toolOutput(path string, args ...string) (string, error) {
	cmd := exec.Command(path, args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", filepath.Base(path), strings.Join(args, " "), err)
	}
	return string(out), nil
}
