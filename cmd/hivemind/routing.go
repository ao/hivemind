package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"time"

	"github.com/ao/hivemind/internal/config"
	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/router"
	"github.com/ao/hivemind/internal/store"
	"github.com/ao/hivemind/internal/tlsstore"
)

// newRouter builds the node's router from the config: its listeners on
// --route-addr, the routing table from routes, and the shared TLS storage on
// backend. The caller serves it, and shuts it down.
func newRouter(cfg *config.Config, log *slog.Logger, routes func(context.Context) (router.Table, error), backend tlsstore.Backend) (*router.Router, error) {
	ts, err := tlsstore.New(backend, tlsstore.Options{Logger: log.With("component", "tls-storage")})
	if err != nil {
		return nil, err
	}
	rt, err := router.New(router.Config{Addr: cfg.RouteAddr, HTTPPort: cfg.RouteHTTPPort, HTTPSPort: cfg.RouteHTTPSPort,
		Routes: routes, Storage: ts, Email: cfg.ACMEEmail, CA: cfg.ACMECA,
		Logger: log.With("component", "router"), Metrics: router.NewMetrics()})
	if err != nil {
		return nil, fmt.Errorf("--route-addr %s: %w", cfg.RouteAddr, err)
	}
	return rt, nil
}

// storeRoutes reads the routing table from the store, for the server's
// router.
func storeRoutes(st *store.Store) func(context.Context) (router.Table, error) {
	return func(ctx context.Context) (router.Table, error) {
		rt, err := st.RouteTable(ctx)
		if err != nil {
			return nil, err
		}
		t := make(router.Table, len(rt))
		for _, r := range rt {
			t[r.Host] = r.Endpoints
		}
		return t, nil
	}
}

// nodeRoutes reads the routing table over the node API, for a worker's
// router. A server without the route (older than 2.2) is asked again only
// when missing lets it.
func nodeRoutes(c *nodeapi.Client, nodeID string, missing *nodeapi.MissingRoute) func(context.Context) (router.Table, error) {
	return func(ctx context.Context) (router.Table, error) {
		if missing.Skip() {
			return nil, nodeapi.ErrNoRoute
		}
		rs, err := c.Routes(ctx, nodeID)
		if err := missing.Check(err); err != nil {
			return nil, err
		}
		t := make(router.Table, len(rs))
		for _, r := range rs {
			t[r.Host] = r.Endpoints
		}
		return t, nil
	}
}

// serverNodeAddress is the private address of the server's own node, where
// its routed replicas publish their ports: the address the cluster listener
// binds, from --advertise. A host name is resolved as net.Listen resolves
// it, to its first IPv4 address if it has one. Without --advertise there are
// no workers, and routed replicas publish on loopback.
func serverNodeAddress(ctx context.Context, advertise string) (string, error) {
	if advertise == "" {
		return "127.0.0.1", nil
	}
	if ip, err := netip.ParseAddr(advertise); err == nil {
		return ip.Unmap().String(), nil
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", advertise)
	if err != nil {
		return "", fmt.Errorf("--advertise %s: %w", advertise, err)
	}
	if len(ips) == 0 {
		return "", fmt.Errorf("--advertise %s resolves to no address", advertise)
	}
	for _, ip := range ips {
		if ip.Unmap().Is4() {
			return ip.Unmap().String(), nil
		}
	}
	return ips[0].String(), nil
}

// checkBoundAddress checks that the cluster listener bound the address the
// server's node registered, since its routed replicas publish there.
func checkBoundAddress(bound, want string) error {
	host, _, err := net.SplitHostPort(bound)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return err
	}
	if ip.Unmap().String() != want {
		return fmt.Errorf("the cluster listener bound %s, but --advertise resolved to %s for routed replicas; give --advertise as an IP address", host, want)
	}
	return nil
}

// workerAddress is a worker's private address: the local address of its
// connection to the server. One that other nodes could not reach, such as
// loopback when the server is on the same machine, is not sent, and the node
// takes no routed replicas; that is logged. An error means the server could
// not be reached, and the caller tries again.
func workerAddress(ctx context.Context, c *nodeapi.Client, nodeID string, log *slog.Logger) (string, error) {
	addr, err := c.LocalAddress(ctx, nodeID)
	if err != nil {
		return "", err
	}
	if err := nodeapi.ValidateNodeAddress(addr); err != nil {
		log.Warn("this node takes no routed replicas: its address to the server is not one other nodes can reach",
			"address", addr, "reason", err.Error())
		return "", nil
	}
	return addr, nil
}

// routerReportEvery is how often a worker posts its router's counters to the
// server, whose /metrics shows them. The server shows a report for two
// minutes after it arrives.
const routerReportEvery = 15 * time.Second

// routerReporter is the part of the node API client that takes a router's
// counters.
type routerReporter interface {
	ReportRouter(ctx context.Context, nodeID string, r nodeapi.RouterReport) error
}

// reportRouter posts report() to the server every period until ctx ends. A
// server older than 2.2 has no route for it; missing logs that once, and
// holds the posts off until the server changes. Other failures are logged
// once until a post works again: the counters are cumulative, so a lost post
// loses nothing.
func reportRouter(ctx context.Context, c routerReporter, nodeID string, report func() nodeapi.RouterReport,
	every time.Duration, missing *nodeapi.MissingRoute, log *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	failing := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if missing.Skip() {
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, every)
		err := missing.Check(c.ReportRouter(pctx, nodeID, report()))
		cancel()
		switch {
		case ctx.Err() != nil:
			return
		case err == nil:
			if failing {
				log.Info("router metrics reach the server again")
			}
			failing = false
		case errors.Is(err, nodeapi.ErrNoRoute):
		case !failing:
			log.Warn("post router metrics to the server; retrying", "err", err)
			failing = true
		}
	}
}
