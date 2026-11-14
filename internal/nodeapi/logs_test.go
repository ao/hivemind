package nodeapi

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestFramesRoundTrip(t *testing.T) {
	var b bytes.Buffer
	fw := NewFrameWriter(&b)
	big := strings.Repeat("x", MaxFramePayload+10)
	if err := fw.WriteFrame(FrameReady, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(fw.Output(FrameStdout), "out 1\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(fw.Output(FrameStderr), "err 1\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(fw.Output(FrameStdout), big); err != nil {
		t.Fatal(err)
	}
	if err := fw.WriteFrame(FrameFailed, []byte(strings.Repeat("m", MaxFrameMessage+100))); err != nil {
		t.Fatal(err)
	}

	fr := NewFrameReader(&b)
	type frame struct {
		t    byte
		body string
	}
	var got []frame
	for {
		typ, _, err := fr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		var sb strings.Builder
		if err := fr.Copy(&sb); err != nil {
			t.Fatal(err)
		}
		got = append(got, frame{typ, sb.String()})
	}
	want := []frame{{FrameReady, ""}, {FrameStdout, "out 1\n"}, {FrameStderr, "err 1\n"},
		{FrameStdout, big[:MaxFramePayload]}, {FrameStdout, big[MaxFramePayload:]},
		{FrameFailed, strings.Repeat("m", MaxFrameMessage)}}
	if len(got) != len(want) {
		t.Fatalf("got %d frames, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("frame %d: type %d, %d bytes; want type %d, %d bytes", i, got[i].t, len(got[i].body), want[i].t, len(want[i].body))
		}
	}
}

func TestFrameReaderSkipsUnreadPayload(t *testing.T) {
	var b bytes.Buffer
	fw := NewFrameWriter(&b)
	_ = fw.WriteFrame(FrameStdout, []byte("skip me"))
	_ = fw.WriteFrame(FrameStderr, []byte("read me"))
	fr := NewFrameReader(&b)
	if _, _, err := fr.Next(); err != nil {
		t.Fatal(err)
	}
	typ, _, err := fr.Next()
	if err != nil || typ != FrameStderr {
		t.Fatalf("type %d, %v", typ, err)
	}
	if msg, _ := fr.Message(); msg != "read me" {
		t.Fatalf("payload %q", msg)
	}
}

func TestFrameReaderRefusesBadFrames(t *testing.T) {
	hdr := func(t byte, reserved byte, n uint32) []byte {
		return []byte{t, reserved, 0, 0, byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}
	}
	cases := map[string][]byte{
		"unknown type":       hdr(9, 0, 0),
		"type zero":          hdr(0, 0, 0),
		"reserved byte set":  hdr(FrameStdout, 1, 0),
		"output too long":    hdr(FrameStdout, 0, MaxFramePayload+1),
		"message too long":   hdr(FrameFailed, 0, MaxFrameMessage+1),
		"length beyond 4GiB": hdr(FrameStderr, 0, 0xffffffff),
	}
	for name, b := range cases {
		if _, _, err := NewFrameReader(bytes.NewReader(b)).Next(); !errors.Is(err, ErrBadFrame) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A header cut short, and a payload cut short.
	if _, _, err := NewFrameReader(bytes.NewReader([]byte{1, 0, 0})).Next(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("short header: %v", err)
	}
	fr := NewFrameReader(bytes.NewReader(append(hdr(FrameStdout, 0, 10), "abc"...)))
	if _, _, err := fr.Next(); err != nil {
		t.Fatal(err)
	}
	if err := fr.Copy(io.Discard); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("short payload: %v", err)
	}
}

func TestLogRequestValidate(t *testing.T) {
	ok := LogRequest{ID: strings.Repeat("a1", 16), App: "web", Replica: 3, Tail: 10}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(*LogRequest){
		"short ID":      func(q *LogRequest) { q.ID = "abc" },
		"uppercase ID":  func(q *LogRequest) { q.ID = strings.Repeat("A1", 16) },
		"path in ID":    func(q *LogRequest) { q.ID = strings.Repeat("a", 30) + "/." },
		"bad app":       func(q *LogRequest) { q.App = "Web_1" },
		"neg replica":   func(q *LogRequest) { q.Replica = -1 },
		"neg tail":      func(q *LogRequest) { q.Tail = -1 },
		"tail too long": func(q *LogRequest) { q.Tail = MaxLogTail + 1 },
	}
	for name, mut := range bad {
		q := ok
		mut(&q)
		if err := q.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
