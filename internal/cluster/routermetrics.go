package cluster

import (
	"net/http"
	"time"

	"github.com/ao/hivemind/internal/nodeapi"
)

// routerMetrics takes a worker's router counters for the server's /metrics.
// Only hosts the server routes are kept, with the unknown host's request
// counts, so a worker cannot add series of its choosing; the server's own
// router's counters do not come this way.
func (a *nodeAPI) routerMetrics(w http.ResponseWriter, r *http.Request) {
	id, ok := a.node(w, r)
	if !ok {
		return
	}
	var rep nodeapi.RouterReport
	if err := nodeapi.Decode(r.Body, nodeapi.MaxRouterReportBytes, &rep); err != nil {
		writeDecodeError(w, err)
		return
	}
	if err := rep.Validate(); err != nil {
		writeDecodeError(w, err)
		return
	}
	rt, err := a.st.RouteTable(r.Context())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	routed := make(map[string]bool, len(rt))
	for _, x := range rt {
		routed[x.Host] = true
	}
	a.routers.Put(id, rep.Routed(routed), time.Now())
	w.WriteHeader(http.StatusNoContent)
}
