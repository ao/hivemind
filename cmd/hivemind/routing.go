package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ao/hivemind/internal/config"
	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/router"
	"github.com/ao/hivemind/internal/store"
	"github.com/ao/hivemind/internal/tlsstore"
)

// newRouter builds the node's router from the config: its listeners on
// --route-addr, the routing table from routes, and the shared TLS storage on
// backend. whole, if set, is closed once the table routes returns has this
// node's own replicas (router.Config.Whole). The caller serves it, and
// shuts it down.
func newRouter(cfg *config.Config, log *slog.Logger, routes func(context.Context) (router.Table, error), backend tlsstore.Backend,
	whole <-chan struct{}) (*router.Router, error) {
	ts, err := tlsstore.New(backend, tlsstore.Options{Logger: log.With("component", "tls-storage")})
	if err != nil {
		return nil, err
	}
	// --route-max-conns-per-ip 0 is no limit, which the router's Config
	// spells as a negative number; its zero is the default.
	perIP := cfg.RouteMaxConnsPerIP
	if perIP == 0 {
		perIP = -1
	}
	rt, err := router.New(router.Config{Addr: cfg.RouteAddr, HTTPPort: cfg.RouteHTTPPort, HTTPSPort: cfg.RouteHTTPSPort,
		Routes: routes, Storage: ts, Email: cfg.ACMEEmail, CA: cfg.ACMECA, MaxConnsPerAddr: perIP, Whole: whole,
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
			t[r.Host] = append(t[r.Host], router.Route{Path: r.Path, Protocol: r.Protocol, Endpoints: r.Endpoints})
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
			t[r.Host] = append(t[r.Host], router.Route{Path: r.Path, Protocol: r.Protocol, Endpoints: r.Endpoints})
		}
		return t, nil
	}
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

// routerOnlySettings are the settings that only a router reads.
var routerOnlySettings = []string{"route-http-port", "route-https-port", "route-max-conns-per-ip", "acme-email", "acme-ca"}

// noteRouterSettings logs, on a node without --route-addr, the router
// settings that were given all the same, with where each came from: the
// node runs no router, so they do nothing.
func noteRouterSettings(cfg *config.Config, log *slog.Logger) {
	if cfg.RouteAddr != "" {
		return
	}
	var ignored []string
	for _, name := range routerOnlySettings {
		if src := cfg.Source(name); src != "" {
			ignored = append(ignored, "--"+name+" ("+src+")")
		}
	}
	if len(ignored) > 0 {
		log.Warn("router settings are ignored without --route-addr: this node runs no router", "ignored", strings.Join(ignored, ", "))
	}
}
