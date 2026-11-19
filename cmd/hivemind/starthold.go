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

// startHoldEndedMessage is the line serve logs, at info, when the hold
// ends because one of the workers reported, and others still wait.
const startHoldEndedMessage = "a worker that was up before this server started has reported to it, which ends the start grace's hold: " +
	"the workers that have not reported yet wait for the start grace again, from that report, until wait_ends; " +
	"their replicas are not placed again until then, and those that have not reported by then are lost"

// startHoldLog logs startHoldMessage once the start grace holds the
// workers, and again every startHoldEvery while it does; and, at info,
// when the hold ends because one of them reported. The sweep calls note
// after each ReconcileNodes, which sees the reports.
type startHoldLog struct {
	logged time.Time // when the line was last logged; zero when it was not
	ended  bool      // the end of the hold was logged
}

func (h *startHoldLog) note(ctx context.Context, st *store.Store, log *slog.Logger, started time.Time) {
	now := st.Now()
	w, ok := st.StartWait(now)
	if !ok {
		return
	}
	if !w.Held {
		if w.HoldEnded.IsZero() || h.ended {
			return
		}
		h.ended, h.logged = true, time.Time{}
		ids, err := waitingIDs(ctx, st, now)
		if err != nil {
			return
		}
		if len(ids) == 0 {
			log.Info("a worker that was up before this server started has reported to it, which ends the start grace's hold; "+
				"every worker that was waiting has reported", "hold_ended", w.HoldEnded.UTC().Format(time.RFC3339))
			return
		}
		log.Info(startHoldEndedMessage, "waiting", len(ids), "nodes", listIDs(ids),
			"hold_ended", w.HoldEnded.UTC().Format(time.RFC3339), "wait_ends", w.WaitEnds.UTC().Format(time.RFC3339))
		return
	}
	if !h.logged.IsZero() && now.Sub(h.logged) < startHoldEvery {
		return
	}
	ids, err := waitingIDs(ctx, st, now)
	if err != nil || len(ids) == 0 {
		// None: every one of them was removed.
		return
	}
	h.logged = now
	log.Error(startHoldMessage, "waiting", len(ids), "nodes", listIDs(ids),
		"started", started.UTC().Format(time.RFC3339), "start_grace_ended", w.GraceEnds.UTC().Format(time.RFC3339))
}

// waitingIDs returns the IDs of the nodes waiting at now.
func waitingIDs(ctx context.Context, st *store.Store, now time.Time) ([]string, error) {
	nodes, err := st.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, n := range nodes {
		if st.Waiting(n, now) {
			ids = append(ids, n.ID)
		}
	}
	return ids, nil
}

// listIDs joins the first startHoldListed of ids, and marks the rest.
func listIDs(ids []string) string {
	listed := ids[:min(len(ids), startHoldListed)]
	more := ""
	if len(ids) > len(listed) {
		more = ",..."
	}
	return strings.Join(listed, ",") + more
}
