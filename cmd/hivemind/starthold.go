package main

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/ao/hivemind/internal/store"
)

// startHoldEvery is how often serve says again that its start grace holds
// the workers, while it does.
const startHoldEvery = time.Hour

// startHoldListed bounds the node IDs the line names.
const startHoldListed = 10

// startHoldMessage is the line serve logs, at error, when its start grace
// has ended with no report from any worker that was waiting (store.StartWait).
const startHoldMessage = "no worker has reported to this server since it started, and its start grace has ended: " +
	"the workers that were up before the start stay waiting, not lost, and their replicas are not placed again. " +
	"An old copy of a cluster that was restored elsewhere sees this, and so does a server cut off from its workers. " +
	"If the cluster was restored on another machine, stop this server and disable its service; " +
	"if a worker is really gone, remove it with hivemind node remove <id>, and its replicas are placed again " +
	"(docs/backup.md#if-the-old-server-comes-back)"

// startHoldLog logs startHoldMessage once the start grace holds the
// workers, and again every startHoldEvery while it does; and, at info,
// when the hold ends because one of them reported. The sweep calls note
// after each ReconcileNodes, which sees the reports.
type startHoldLog struct {
	logged time.Time // when the line was last logged; zero when it was not
}

func (h *startHoldLog) note(ctx context.Context, st *store.Store, log *slog.Logger, started time.Time) {
	now := st.Now()
	w, ok := st.StartWait(now)
	if !ok {
		return
	}
	if !w.Held {
		if !h.logged.IsZero() {
			h.logged = time.Time{}
			log.Info("a worker that was up before this server started has reported to it: the start grace holds the others no longer, " +
				"and those that have not reported are lost")
		}
		return
	}
	if !h.logged.IsZero() && now.Sub(h.logged) < startHoldEvery {
		return
	}
	nodes, err := st.ListNodes(ctx)
	if err != nil {
		return
	}
	var ids []string
	for _, n := range nodes {
		if st.Waiting(n, now) {
			ids = append(ids, n.ID)
		}
	}
	if len(ids) == 0 {
		// Every one of them was removed.
		return
	}
	h.logged = now
	listed := ids[:min(len(ids), startHoldListed)]
	more := ""
	if len(ids) > len(listed) {
		more = ",..."
	}
	log.Error(startHoldMessage, "waiting", len(ids), "nodes", strings.Join(listed, ",")+more,
		"started", started.UTC().Format(time.RFC3339), "start_grace_ended", w.GraceEnds.UTC().Format(time.RFC3339))
}
