package ha

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/raft"
)

// TestLogStore: entries keep every field; first and last follow writes and
// deletes; a missing entry is raft.ErrLogNotFound; the stable store gives
// empty values for keys never set, and the file is mode 0600.
func TestLogStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "raft", logFile)
	s, err := openLogStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("raft.db mode: %v %v", fi.Mode(), err)
	}
	if first, err := s.FirstIndex(); err != nil || first != 0 {
		t.Fatalf("empty first index: %d %v", first, err)
	}
	if last, err := s.LastIndex(); err != nil || last != 0 {
		t.Fatalf("empty last index: %d %v", last, err)
	}
	when := time.Unix(1_700_000_000, 123)
	var logs []*raft.Log
	for i := uint64(1); i <= 10; i++ {
		logs = append(logs, &raft.Log{Index: i, Term: i / 3, Type: raft.LogCommand, Data: []byte{byte(i), 0, 0xff},
			Extensions: []byte("x"), AppendedAt: when})
	}
	logs[4].Data = nil
	if err := s.StoreLogs(logs); err != nil {
		t.Fatal(err)
	}
	if err := s.StoreLog(&raft.Log{Index: 11, Term: 4, Type: raft.LogConfiguration, Data: []byte("c")}); err != nil {
		t.Fatal(err)
	}
	var got raft.Log
	if err := s.GetLog(3, &got); err != nil {
		t.Fatal(err)
	}
	if got.Index != 3 || got.Term != 1 || got.Type != raft.LogCommand || !bytes.Equal(got.Data, []byte{3, 0, 0xff}) ||
		string(got.Extensions) != "x" || !got.AppendedAt.Equal(when) {
		t.Fatalf("entry 3: %+v", got)
	}
	if err := s.GetLog(11, &got); err != nil || got.Type != raft.LogConfiguration || !got.AppendedAt.IsZero() {
		t.Fatalf("entry 11: %+v %v", got, err)
	}
	if err := s.GetLog(12, &got); !errors.Is(err, raft.ErrLogNotFound) {
		t.Fatalf("missing entry: %v", err)
	}
	if err := s.DeleteRange(1, 4); err != nil {
		t.Fatal(err)
	}
	if first, _ := s.FirstIndex(); first != 5 {
		t.Fatalf("first after deleting 1-4: %d", first)
	}
	if err := s.DeleteRange(9, 1<<64-1); err != nil {
		t.Fatal(err)
	}
	if last, _ := s.LastIndex(); last != 8 {
		t.Fatalf("last after deleting from 9: %d", last)
	}
	if v, err := s.Get([]byte("CurrentTerm")); err != nil || len(v) != 0 {
		t.Fatalf("a key never set: %q %v", v, err)
	}
	if v, err := s.GetUint64([]byte("LastVoteTerm")); err != nil || v != 0 {
		t.Fatalf("a number never set: %d %v", v, err)
	}
	if err := s.SetUint64([]byte("CurrentTerm"), 1<<40+7); err != nil {
		t.Fatal(err)
	}
	if err := s.Set([]byte("LastVoteCand"), []byte("node-a")); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = openLogStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v, err := s.GetUint64([]byte("CurrentTerm")); err != nil || v != 1<<40+7 {
		t.Fatalf("term after a reopen: %d %v", v, err)
	}
	if v, err := s.Get([]byte("LastVoteCand")); err != nil || string(v) != "node-a" {
		t.Fatalf("vote after a reopen: %q %v", v, err)
	}
	if first, _ := s.FirstIndex(); first != 5 {
		t.Fatalf("first after a reopen: %d", first)
	}
}
