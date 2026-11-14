package scheduler

import (
	"net/netip"
	"strconv"
	"strings"
)

// hostPort is one parsed Replica.HostPorts entry.
type hostPort struct {
	raw string
	// key is "port/proto". Two entries can only conflict if their keys match.
	key string
	// addr is the bind address. It is invalid when the entry gave none,
	// which means every address.
	addr netip.Addr
	// opaque entries could not be parsed and conflict only with an
	// identical entry.
	opaque bool
}

func parsePorts(list []string) []hostPort {
	out := make([]hostPort, 0, len(list))
	for _, s := range list {
		out = append(out, parsePort(s))
	}
	return out
}

func parsePort(s string) hostPort {
	opaque := hostPort{raw: s, key: s, opaque: true}
	slash := strings.LastIndexByte(s, '/')
	if slash < 0 {
		return opaque
	}
	proto := strings.ToLower(s[slash+1:])
	left := s[:slash]
	if proto == "" {
		return opaque
	}
	var addr netip.Addr
	var port int
	if strings.ContainsRune(left, ':') {
		ap, err := netip.ParseAddrPort(left)
		if err != nil {
			return opaque
		}
		addr, port = ap.Addr().Unmap().WithZone(""), int(ap.Port())
	} else {
		n, err := strconv.Atoi(left)
		if err != nil || n < 0 || n > 65535 {
			return opaque
		}
		port = n
	}
	return hostPort{raw: s, key: strconv.Itoa(port) + "/" + proto, addr: addr}
}

// conflicts reports whether two published ports cannot share a node.
// 127.0.0.1:80/tcp and 0.0.0.0:80/tcp conflict; 10.0.0.1:80/tcp and
// 10.0.0.2:80/tcp do not. 0.0.0.0 covers every IPv4 address, and ::
// covers every address.
func (p hostPort) conflicts(q hostPort) bool {
	if p.opaque || q.opaque {
		return p.opaque && q.opaque && p.raw == q.raw
	}
	if p.key != q.key {
		return false
	}
	if !p.addr.IsValid() || !q.addr.IsValid() || p.addr == q.addr {
		return true
	}
	return covers(p.addr, q.addr) || covers(q.addr, p.addr)
}

// covers reports whether a wildcard bind on a also takes b.
func covers(a, b netip.Addr) bool {
	if !a.IsUnspecified() {
		return false
	}
	if a.Is6() {
		return true
	}
	return b.Is4() || b.IsUnspecified()
}
