package docker

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
)

// frame builds one multiplexed frame: the stream type, three reserved bytes,
// and the payload length as a big-endian uint32, then the payload.
func frame(typ byte, payload string) []byte {
	b := make([]byte, 8, 8+len(payload))
	b[0] = typ
	binary.BigEndian.PutUint32(b[4:8], uint32(len(payload)))
	return append(b, payload...)
}

func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

func TestStdCopy(t *testing.T) {
	tests := []struct {
		name       string
		in         []byte
		wantOut    string
		wantErrOut string
		wantIs     error  // errors.Is target, if the stream is invalid
		wantSub    string // substring of the error message, if the stream is invalid
	}{
		{name: "empty input"},
		{
			name:       "stdout and stderr interleaved",
			in:         cat(frame(1, "out one\n"), frame(2, "err one\n"), frame(1, "out two\n")),
			wantOut:    "out one\nout two\n",
			wantErrOut: "err one\n",
		},
		{name: "stdin frames go to stdout", in: frame(0, "typed"), wantOut: "typed"},
		{name: "system frames are discarded", in: cat(frame(3, "internal"), frame(1, "kept")), wantOut: "kept"},
		{name: "empty payload", in: cat(frame(1, ""), frame(1, "x")), wantOut: "x"},
		{name: "partial header", in: frame(1, "abc")[:5], wantIs: io.ErrUnexpectedEOF},
		{name: "payload cut short", in: frame(1, "abcdef")[:10], wantOut: "ab", wantIs: io.ErrUnexpectedEOF},
		{
			name:   "declared length larger than the stream",
			in:     []byte{1, 0, 0, 0, 0xff, 0xff, 0xff, 0xff},
			wantIs: io.ErrUnexpectedEOF,
		},
		{
			name:    "unknown stream type",
			in:      cat(frame(1, "ok"), frame(9, "bad")),
			wantOut: "ok",
			wantSub: "unknown stream type 9",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			err := StdCopy(&out, &errOut, bytes.NewReader(tc.in))
			switch {
			case tc.wantIs != nil:
				if !errors.Is(err, tc.wantIs) {
					t.Fatalf("err = %v, want %v", err, tc.wantIs)
				}
			case tc.wantSub != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantSub)
				}
			default:
				if err != nil {
					t.Fatalf("err = %v", err)
				}
			}
			if out.String() != tc.wantOut {
				t.Errorf("stdout = %q, want %q", out.String(), tc.wantOut)
			}
			if errOut.String() != tc.wantErrOut {
				t.Errorf("stderr = %q, want %q", errOut.String(), tc.wantErrOut)
			}
		})
	}
}

// reference is an independent parser used by FuzzStdCopy. It returns the
// bytes StdCopy must write, or the first error in stream order.
func reference(data []byte) (stdout, stderr []byte, err error) {
	for i := 0; i < len(data); {
		if len(data)-i < 8 {
			return nil, nil, io.ErrUnexpectedEOF
		}
		typ := data[i]
		size := int(binary.BigEndian.Uint32(data[i+4 : i+8]))
		i += 8
		if typ > 3 {
			return nil, nil, errors.New("unknown stream type")
		}
		if size > len(data)-i {
			return nil, nil, io.ErrUnexpectedEOF
		}
		payload := data[i : i+size]
		switch typ {
		case 0, 1:
			stdout = append(stdout, payload...)
		case 2:
			stderr = append(stderr, payload...)
		}
		i += size
	}
	return stdout, stderr, nil
}

// FuzzStdCopy checks StdCopy against the reference parser on arbitrary bytes:
// the same output for valid streams, an error for truncated or unknown ones,
// and no panic.
func FuzzStdCopy(f *testing.F) {
	f.Add(cat(frame(1, "out\n"), frame(2, "err\n")))
	f.Add(frame(1, "")[:3])
	f.Add([]byte{1, 0, 0, 0, 0xff, 0xff, 0xff, 0xff})
	f.Add(cat(frame(9, "x")))
	f.Add(cat(frame(3, "sys"), frame(0, "in")))
	f.Fuzz(func(t *testing.T, data []byte) {
		var out, errOut bytes.Buffer
		err := StdCopy(&out, &errOut, bytes.NewReader(data))
		wantOut, wantErr, refErr := reference(data)
		if refErr != nil {
			if err == nil {
				t.Fatalf("StdCopy returned nil for an invalid stream (%v)", refErr)
			}
			if errors.Is(refErr, io.ErrUnexpectedEOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("StdCopy error %v, want io.ErrUnexpectedEOF", err)
			}
			return
		}
		if err != nil {
			t.Fatalf("StdCopy error %v for a valid stream", err)
		}
		if !bytes.Equal(out.Bytes(), wantOut) {
			t.Fatalf("stdout = %q, want %q", out.Bytes(), wantOut)
		}
		if !bytes.Equal(errOut.Bytes(), wantErr) {
			t.Fatalf("stderr = %q, want %q", errOut.Bytes(), wantErr)
		}
	})
}
