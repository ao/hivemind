package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/ao/hivemind/internal/config"
	"github.com/ao/hivemind/internal/store"
)

// resolveTimeout bounds serve's lookup of an --advertise name.
const resolveTimeout = 10 * time.Second

// addrSource lists the IP addresses of this machine. Tests replace it.
type addrSource func() ([]netip.Addr, error)

// interfaceAddrs is addrSource for this machine: the address of each
// interface, without its prefix length. Zoned (link-local IPv6) addresses
// are left out; the cluster listeners never bind one.
func interfaceAddrs() ([]netip.Addr, error) {
	as, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	out := make([]netip.Addr, 0, len(as))
	for _, a := range as {
		if p, err := netip.ParsePrefix(a.String()); err == nil {
			out = append(out, p.Addr().Unmap())
		}
	}
	return out, nil
}

// resolver looks a name up. Tests replace it.
type resolver func(ctx context.Context, name string) ([]netip.Addr, error)

func lookupName(ctx context.Context, name string) ([]netip.Addr, error) {
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	return net.DefaultResolver.LookupNetIP(ctx, "ip", name)
}

// clusterAddress is the address the server's cluster listeners bind, and the
// private address of its own node, where its routed replicas publish their
// ports:
//
//   - --cluster-addr, when it is given;
//   - else --advertise, when it is an IP address;
//   - else the address of this machine that the --advertise name resolves
//     to: a non-loopback IPv4 address first, then a non-loopback IPv6 one,
//     then loopback, which is logged, since no other machine reaches it.
//
// --cluster-addr, and --advertise when it is an IP address, must be an
// address of this machine (onThisMachine): one that is not is refused with
// this machine's addresses, before serve records anything or binds.
// A name that resolves to no address of this machine is refused, with what
// to do: its record names another machine, as it does after a restore until
// it is updated, and --cluster-addr lets the server start before then. With
// --cluster-addr, a name that does not resolve to it is logged and allowed.
// Without --advertise there is no cluster listener and no worker, and routed
// replicas publish on loopback.
func clusterAddress(ctx context.Context, cfg *config.Config, local addrSource, lookup resolver, log *slog.Logger) (string, error) {
	if cfg.Advertise == "" {
		if cfg.ClusterAddr != "" {
			return "", fmt.Errorf("--cluster-addr %s needs --advertise, the address workers dial: without it there is no cluster listener", cfg.ClusterAddr)
		}
		return "127.0.0.1", nil
	}
	advIP, advErr := netip.ParseAddr(cfg.Advertise)
	if cfg.ClusterAddr != "" {
		bind, err := netip.ParseAddr(cfg.ClusterAddr) // config.Validate checked it
		if err != nil {
			return "", fmt.Errorf("--cluster-addr %q: %w", cfg.ClusterAddr, err)
		}
		bind = bind.Unmap()
		if err := onThisMachine("--cluster-addr", bind, local, log); err != nil {
			return "", fmt.Errorf("%w. Give one of them", err)
		}
		if advErr != nil {
			ips, err := lookup(ctx, cfg.Advertise)
			switch {
			case err != nil:
				log.Warn("--advertise does not resolve: workers reach this server once it resolves to --cluster-addr, or to an address that leads to it",
					"advertise", cfg.Advertise, "cluster_addr", bind.String(), "err", err)
			case !slices.Contains(unmapAll(ips), bind):
				log.Warn("--advertise resolves to other addresses than --cluster-addr: workers reach this server only once its DNS record names "+
					"--cluster-addr, or an address that leads to it", "advertise", cfg.Advertise, "resolves_to", addrList(ips), "cluster_addr", bind.String())
			}
		}
		return bind.String(), nil
	}
	if advErr == nil {
		ip := advIP.Unmap()
		if err := onThisMachine("--advertise", ip, local, log); err != nil {
			return "", fmt.Errorf("%w. Give one of them, or, for an address that leads to this machine from elsewhere, "+
				"as a NAT's, add this machine's address with --cluster-addr", err)
		}
		return ip.String(), nil
	}
	ips, err := lookup(ctx, cfg.Advertise)
	if err != nil {
		return "", fmt.Errorf("--advertise %s: %w; give this machine's address with --cluster-addr to start before the name resolves", cfg.Advertise, err)
	}
	if len(ips) == 0 {
		return "", fmt.Errorf("--advertise %s resolves to no address; give this machine's address with --cluster-addr to start before it does", cfg.Advertise)
	}
	mine, err := local()
	if err != nil {
		// Without the list, bind what the name resolves to, as net.Listen
		// would; a wrong address fails there.
		log.Warn("listing this machine's addresses", "err", err)
		return preferred(unmapAll(ips)).String(), nil
	}
	var here []netip.Addr
	for _, ip := range unmapAll(ips) {
		if ip.IsLoopback() || slices.Contains(mine, ip) {
			here = append(here, ip)
		}
	}
	if len(here) == 0 {
		return "", fmt.Errorf("--advertise %s resolves to %s, and this machine has none of those addresses, so the cluster listeners cannot bind it. "+
			"If the name's DNS record still names another machine, as after a restore until the record is updated, "+
			"give this machine's address with --cluster-addr (see docs/backup.md)", cfg.Advertise, addrList(ips))
	}
	bind := preferred(here)
	if bind.IsLoopback() {
		log.Warn("--advertise resolves to a loopback address here, so no other machine reaches the cluster listeners: "+
			"give this machine's address with --cluster-addr, or fix the name's record (a hosts file may map this machine's name to 127.0.1.1)",
			"advertise", cfg.Advertise, "cluster_addr", bind.String())
	}
	return bind.String(), nil
}

// onThisMachine checks that ip, given with flag, is an address of this
// machine, which the cluster listeners can bind. A loopback address is one,
// as Linux routes all of 127/8 to loopback. When this machine's addresses
// cannot be listed, that is logged and ip is taken: a wrong one then fails
// at the bind.
func onThisMachine(flag string, ip netip.Addr, local addrSource, log *slog.Logger) error {
	if ip.IsLoopback() {
		return nil
	}
	mine, err := local()
	if err != nil {
		log.Warn("listing this machine's addresses", "err", err)
		return nil
	}
	if slices.Contains(mine, ip) {
		return nil
	}
	var shown []netip.Addr
	for _, a := range mine {
		if !a.IsLinkLocalUnicast() {
			shown = append(shown, a)
		}
	}
	return fmt.Errorf("%s %s is not an address of this machine, so the cluster listeners cannot bind it; this machine's addresses are %s",
		flag, ip, addrList(shown))
}

// noteAddressChange logs what the workers need when the server's node had
// another address before this start, as after a restore on a new machine,
// and the store has workers. A worker dials the address it joined. One that
// joined a DNS name follows its record, which is logged at info. One that
// joined the old IP address cannot reach this server until it is moved with
// node set-server, which is a warning; that is when --advertise is the
// address the listeners bind, so that the workers dialled the old one.
func noteAddressChange(ctx context.Context, st *store.Store, cfg *config.Config, serverID, addr string, log *slog.Logger) {
	if cfg.Advertise == "" {
		return
	}
	prev, err := st.GetNode(ctx, serverID)
	if err != nil || prev.Address == "" || prev.Address == addr {
		return
	}
	nodes, err := st.ListNodes(ctx)
	if err != nil {
		return
	}
	workers := 0
	for _, n := range nodes {
		if n.ID != serverID {
			workers++
		}
	}
	if workers == 0 {
		return
	}
	if ip, err := netip.ParseAddr(cfg.Advertise); err == nil && ip.Unmap().String() == addr {
		log.Warn("this server's address changed since its last start: workers that joined the old address still dial it, and reach this server "+
			"only once each is moved, with the worker stopped, by hivemind node set-server "+cfg.Advertise+" (docs/backup.md)",
			"old_address", prev.Address, "address", addr, "workers", workers)
		return
	}
	log.Info("this server's address changed since its last start: workers that dial --advertise reach it once that leads to the new address",
		"advertise", cfg.Advertise, "old_address", prev.Address, "address", addr, "workers", workers)
}

// preferred picks a non-loopback IPv4 address, then a non-loopback IPv6 one,
// then an IPv4 one, then the first. ips is not empty.
func preferred(ips []netip.Addr) netip.Addr {
	for _, want := range []func(netip.Addr) bool{
		func(a netip.Addr) bool { return a.Is4() && !a.IsLoopback() },
		func(a netip.Addr) bool { return !a.IsLoopback() },
		netip.Addr.Is4,
	} {
		if i := slices.IndexFunc(ips, want); i >= 0 {
			return ips[i]
		}
	}
	return ips[0]
}

func unmapAll(ips []netip.Addr) []netip.Addr {
	out := make([]netip.Addr, len(ips))
	for i, ip := range ips {
		out[i] = ip.Unmap()
	}
	return out
}

// addrList prints addresses as "10.0.0.5, 10.0.0.6".
func addrList(ips []netip.Addr) string {
	s := make([]string, len(ips))
	for i, ip := range ips {
		s[i] = ip.Unmap().String()
	}
	return strings.Join(s, ", ")
}
