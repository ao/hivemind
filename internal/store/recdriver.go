package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// The write connection's driver. It is the sqlite driver with one thing
// added: while a recorder is set on the connection, each write statement
// that succeeds is recorded with its arguments. A replicated store runs a
// transaction with a recorder, rolls it back, and makes the recorded
// statements one entry of the Raft log, which every server then applies in
// a transaction of its own (docs/ha.md). Without a recorder, the
// connection is the sqlite driver's, call for call.

// writeDriverName is the name the driver is registered under.
const writeDriverName = "hivemind-sqlite-write"

func init() {
	// sql.Open makes no connection: it only finds the registered driver.
	db, err := sql.Open("sqlite", "")
	if err != nil {
		panic(err)
	}
	inner := db.Driver()
	_ = db.Close()
	sql.Register(writeDriverName, &recDriver{inner: inner})
}

type recDriver struct{ inner driver.Driver }

func (d *recDriver) Open(name string) (driver.Conn, error) {
	c, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	rc := &recConn{inner: c}
	var ok bool
	if rc.prep, ok = c.(driver.ConnPrepareContext); !ok {
		c.Close()
		return nil, errors.New("store: the sqlite driver has no PrepareContext")
	}
	if rc.begin, ok = c.(driver.ConnBeginTx); !ok {
		c.Close()
		return nil, errors.New("store: the sqlite driver has no BeginTx")
	}
	rc.exec, _ = c.(driver.ExecerContext)
	rc.query, _ = c.(driver.QueryerContext)
	return rc, nil
}

// recConn is one connection of the write driver. database/sql uses a
// connection from one goroutine at a time, so rec needs no lock.
type recConn struct {
	inner driver.Conn
	prep  driver.ConnPrepareContext
	begin driver.ConnBeginTx
	exec  driver.ExecerContext  // nil: database/sql prepares instead
	query driver.QueryerContext // nil: database/sql prepares instead
	rec   *recorder
}

var (
	_ driver.Conn               = (*recConn)(nil)
	_ driver.ConnPrepareContext = (*recConn)(nil)
	_ driver.ConnBeginTx        = (*recConn)(nil)
	_ driver.ExecerContext      = (*recConn)(nil)
	_ driver.QueryerContext     = (*recConn)(nil)
	_ driver.Pinger             = (*recConn)(nil)
	_ driver.SessionResetter    = (*recConn)(nil)
	_ driver.Validator          = (*recConn)(nil)
)

func (c *recConn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

func (c *recConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	s, err := c.prep.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	return &recStmt{inner: s, conn: c, query: query}, nil
}

func (c *recConn) Close() error { return c.inner.Close() }

func (c *recConn) Begin() (driver.Tx, error) {
	return c.begin.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *recConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.begin.BeginTx(ctx, opts)
}

func (c *recConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if c.exec == nil {
		return nil, driver.ErrSkip
	}
	res, err := c.exec.ExecContext(ctx, query, args)
	if err == nil && c.rec != nil {
		c.rec.add(query, args, res)
	}
	return res, err
}

func (c *recConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if c.query == nil {
		return nil, driver.ErrSkip
	}
	if c.rec != nil {
		if err := refuseQueryWrite(query); err != nil {
			return nil, err
		}
	}
	return c.query.QueryContext(ctx, query, args)
}

func (c *recConn) Ping(ctx context.Context) error {
	if p, ok := c.inner.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

func (c *recConn) ResetSession(ctx context.Context) error {
	if r, ok := c.inner.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

func (c *recConn) IsValid() bool {
	if v, ok := c.inner.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

// recStmt is a prepared statement of the write driver, recorded with the
// text it was prepared from each time it runs.
type recStmt struct {
	inner driver.Stmt
	conn  *recConn
	query string
}

func (s *recStmt) Close() error  { return s.inner.Close() }
func (s *recStmt) NumInput() int { return s.inner.NumInput() }

func (s *recStmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), namedValues(args))
}

func (s *recStmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), namedValues(args))
}

func (s *recStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	ex, ok := s.inner.(driver.StmtExecContext)
	if !ok {
		return nil, errors.New("store: the sqlite driver's statement has no ExecContext")
	}
	res, err := ex.ExecContext(ctx, args)
	if err == nil && s.conn.rec != nil {
		s.conn.rec.add(s.query, args, res)
	}
	return res, err
}

func (s *recStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	q, ok := s.inner.(driver.StmtQueryContext)
	if !ok {
		return nil, errors.New("store: the sqlite driver's statement has no QueryContext")
	}
	if s.conn.rec != nil {
		if err := refuseQueryWrite(s.query); err != nil {
			return nil, err
		}
	}
	return q.QueryContext(ctx, args)
}

func namedValues(args []driver.Value) []driver.NamedValue {
	out := make([]driver.NamedValue, len(args))
	for i, v := range args {
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return out
}

// queryWrite matches a statement that writes. A write through a query
// would not be recorded, so a recording connection refuses one: there is
// none in the store, and this keeps it so.
var queryWrite = regexp.MustCompile(`(?i)^\s*(insert|update|delete|replace|create|drop|alter|vacuum|attach|detach|reindex|analyze)\b|\b(insert\s+into|update\s+\w+\s+set|delete\s+from|returning)\b`)

func refuseQueryWrite(query string) error {
	if queryWrite.MatchString(query) {
		return fmt.Errorf("store: a write run as a query would not be replicated: %s", firstSQLLine(query))
	}
	return nil
}

func firstSQLLine(q string) string {
	q = strings.TrimSpace(q)
	if i := strings.IndexByte(q, '\n'); i >= 0 {
		return q[:i] + " ..."
	}
	return q
}

// recorder collects the write statements of one transaction that changed
// a row. A single INSERT, UPDATE, DELETE or REPLACE that changed none is
// left out: it changed nothing of the state the leader ran it on, and
// every server applies the batch to that same state, statement by
// statement, so it would change nothing there either (docs/ha.md). In the
// eighth soak, such statements, the cleanup DELETEs of every report, were
// 18 % of the Raft log's bytes. SQLite counts a row an UPDATE matched even
// when it sets the values the row has, so an UPDATE that may set the same
// values says so in its WHERE.
type recorder struct {
	stmts []Statement
	// wrote is true once a write statement has run, recorded or not: a
	// transaction that ran one needs the leadership, as before, even when
	// it changed nothing.
	wrote bool
	// changed is true once a statement has changed a row.
	changed bool
	// err is the first statement that cannot be recorded, as one with a
	// named argument. The transaction fails with it.
	err error
}

func (r *recorder) add(query string, args []driver.NamedValue, res driver.Result) {
	vals := make([]any, len(args))
	for i, a := range args {
		if a.Name != "" && r.err == nil {
			r.err = fmt.Errorf("store: statement with a named argument (%s) cannot be recorded: %s", a.Name, firstSQLLine(query))
		}
		switch v := a.Value.(type) {
		case []byte:
			// The driver binds a nil slice as NULL and an empty one as an
			// empty blob; the batch keeps the difference.
			if v == nil {
				vals[i] = nil
			} else {
				vals[i] = append([]byte{}, v...)
			}
		case nil, int64, float64, bool, string, time.Time:
			vals[i] = v
		default:
			if r.err == nil {
				r.err = fmt.Errorf("store: argument %d of type %T cannot be recorded: %s", i+1, v, firstSQLLine(query))
			}
		}
	}
	r.wrote = true
	n, err := res.RowsAffected()
	if err == nil && n == 0 && singleDML(query) {
		return
	}
	r.stmts = append(r.stmts, Statement{SQL: query, Args: vals})
	if err == nil && n > 0 {
		r.changed = true
	}
}

// singleDML reports whether query is one INSERT, UPDATE, DELETE or
// REPLACE statement, whose result's rows affected are its own: the driver
// gives the last statement's for several, and a statement of another kind
// leaves the count of the one before.
func singleDML(query string) bool {
	q := strings.TrimSuffix(strings.TrimSpace(query), ";")
	if strings.Contains(q, ";") {
		return false
	}
	i := strings.IndexFunc(q, unicode.IsSpace)
	if i < 0 {
		return false
	}
	switch strings.ToUpper(q[:i]) {
	case "INSERT", "UPDATE", "DELETE", "REPLACE":
		return true
	}
	return false
}

// setRecorder sets rec on the write connection conn holds, nil to stop.
func setRecorder(conn *sql.Conn, rec *recorder) error {
	return conn.Raw(func(dc any) error {
		c, ok := dc.(*recConn)
		if !ok {
			return fmt.Errorf("store: the write connection is a %T, not the recording driver's", dc)
		}
		c.rec = rec
		return nil
	})
}
