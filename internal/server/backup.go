package server

import (
	"net"
	"net/http"
	"net/netip"
	"time"
)

// backupWriteTimeout bounds the response of POST /v1/backup, in place of
// WriteTimeout: a store of some hundreds of megabytes takes longer than a
// minute on a slow link.
const backupWriteTimeout = 30 * time.Minute

// backupRefusedPlaintext is the reply to a backup request that came over
// plain HTTP from another machine.
const backupRefusedPlaintext = "a backup holds the cluster CA's private key, so the API sends one only over TLS or to a loopback client"

// postBackup streams a backup of the server's data directory: the store, the
// cluster CA with its private key, and the admin token file. It needs the
// admin token like every route. The archive leaves the machine only over
// TLS: on a plain-HTTP API, which listens on loopback only, a client that is
// not on loopback is refused as well, in case the API sits behind a proxy.
//
// The snapshot is taken before the reply starts, so a failure there is an
// error reply. A failure once the archive is streaming aborts the
// connection, so the client sees a truncated transfer rather than a
// complete-looking file; the CLI checks every archive before it keeps it.
func (s *Server) postBackup(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Backup == nil {
		writeError(w, http.StatusServiceUnavailable, "this server cannot write a backup")
		return
	}
	if r.TLS == nil && !loopbackPeer(r.RemoteAddr) {
		s.log.Warn("backup refused: plain HTTP from a client that is not on loopback", "remote", r.RemoteAddr)
		writeError(w, http.StatusForbidden, backupRefusedPlaintext)
		return
	}
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(backupWriteTimeout)); err != nil {
		s.log.Warn("extend write deadline for a backup", "err", err)
	}
	bw := &backupWriter{w: w}
	err := s.cfg.Backup(r.Context(), bw)
	switch {
	case err == nil:
		s.log.Info("backup sent", "remote", r.RemoteAddr, "bytes", bw.n)
	case bw.n == 0:
		s.log.Error("backup failed", "err", err)
		writeError(w, http.StatusInternalServerError, "the backup failed; see the server log")
	default:
		s.log.Error("backup failed while it was sent; the connection is aborted", "remote", r.RemoteAddr, "bytes", bw.n, "err", err)
		panic(http.ErrAbortHandler)
	}
}

// backupWriter sends the headers on the first write, so an error before any
// data can still be an error reply.
type backupWriter struct {
	w http.ResponseWriter
	n int64
}

func (b *backupWriter) Write(p []byte) (int, error) {
	if b.n == 0 && len(p) > 0 {
		h := b.w.Header()
		h.Set("Content-Type", "application/x-tar")
		h.Set("Content-Disposition", `attachment; filename="hivemind-backup.tar"`)
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		b.w.WriteHeader(http.StatusOK)
	}
	n, err := b.w.Write(p)
	b.n += int64(n)
	return n, err
}

// loopbackPeer reports whether a request's remote address is a loopback IP.
func loopbackPeer(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.Unmap().IsLoopback()
}
