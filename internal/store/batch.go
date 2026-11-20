package store

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"
)

// A batch is the write statements of one transaction, in the order it ran
// them, with their arguments: one entry of the Raft log (docs/ha.md). The
// encoding is exact: integers stay integers, floats keep their bits, and
// strings and blobs their bytes, so every server runs the statements the
// leader ran.

// Batch is one transaction's writes.
type Batch struct {
	// Schema is the schema version of the store that recorded it. A store
	// at another version refuses it.
	Schema int
	// Origin names the process that recorded it, and Seq is its number in
	// that process. The store that wrote a batch knows, when it applies it,
	// that the batch is the one its transaction waits for.
	Origin string
	Seq    uint64
	Stmts  []Statement
}

// Statement is one write statement and its arguments, each nil, int64,
// float64, bool, []byte, string or time.Time.
type Statement struct {
	SQL  string
	Args []any
}

// batchMagic starts every encoded batch; the last byte is the format.
var batchMagic = []byte("HMB1")

// Argument types in an encoded batch.
const (
	argNil byte = iota
	argInt
	argFloat
	argBool
	argBytes
	argString
	argTime
)

// maxBatchCount bounds the counts a decoder accepts before it allocates.
const maxBatchCount = 1 << 24

// EncodeBatch encodes b.
func EncodeBatch(b Batch) ([]byte, error) {
	out := append([]byte{}, batchMagic...)
	out = binary.AppendUvarint(out, uint64(b.Schema))
	out = appendString(out, b.Origin)
	out = binary.AppendUvarint(out, b.Seq)
	out = binary.AppendUvarint(out, uint64(len(b.Stmts)))
	for _, st := range b.Stmts {
		out = appendString(out, st.SQL)
		out = binary.AppendUvarint(out, uint64(len(st.Args)))
		for i, a := range st.Args {
			switch v := a.(type) {
			case nil:
				out = append(out, argNil)
			case int64:
				out = append(out, argInt)
				out = binary.AppendVarint(out, v)
			case float64:
				out = append(out, argFloat)
				out = binary.LittleEndian.AppendUint64(out, math.Float64bits(v))
			case bool:
				out = append(out, argBool)
				if v {
					out = append(out, 1)
				} else {
					out = append(out, 0)
				}
			case []byte:
				out = append(out, argBytes)
				out = appendString(out, string(v))
			case string:
				out = append(out, argString)
				out = appendString(out, v)
			case time.Time:
				t, err := v.MarshalBinary()
				if err != nil {
					return nil, fmt.Errorf("store: argument %d: %w", i+1, err)
				}
				out = append(out, argTime)
				out = appendString(out, string(t))
			default:
				return nil, fmt.Errorf("store: argument %d of type %T cannot be encoded", i+1, a)
			}
		}
	}
	return out, nil
}

func appendString(out []byte, s string) []byte {
	out = binary.AppendUvarint(out, uint64(len(s)))
	return append(out, s...)
}

// errBadBatch is a batch that does not decode.
var errBadBatch = errors.New("store: malformed batch")

// DecodeBatch decodes what EncodeBatch made.
func DecodeBatch(data []byte) (Batch, error) {
	d := batchDecoder{b: data}
	if len(data) < len(batchMagic) || string(data[:len(batchMagic)]) != string(batchMagic) {
		return Batch{}, fmt.Errorf("%w: no header", errBadBatch)
	}
	d.b = data[len(batchMagic):]
	var b Batch
	b.Schema = int(d.uvarint())
	b.Origin = d.str()
	b.Seq = d.uvarint()
	n := d.count()
	if d.err == nil && n > 0 {
		b.Stmts = make([]Statement, 0, min(n, 1024))
	}
	for i := 0; i < n && d.err == nil; i++ {
		st := Statement{SQL: d.str()}
		na := d.count()
		if na > 0 && d.err == nil {
			st.Args = make([]any, 0, min(na, 1024))
		}
		for j := 0; j < na && d.err == nil; j++ {
			st.Args = append(st.Args, d.arg())
		}
		b.Stmts = append(b.Stmts, st)
	}
	if d.err != nil {
		return Batch{}, d.err
	}
	if len(d.b) != 0 {
		return Batch{}, fmt.Errorf("%w: %d bytes after the end", errBadBatch, len(d.b))
	}
	return b, nil
}

type batchDecoder struct {
	b   []byte
	err error
}

func (d *batchDecoder) fail(what string) {
	if d.err == nil {
		d.err = fmt.Errorf("%w: %s", errBadBatch, what)
	}
}

func (d *batchDecoder) uvarint() uint64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Uvarint(d.b)
	if n <= 0 {
		d.fail("bad number")
		return 0
	}
	d.b = d.b[n:]
	return v
}

func (d *batchDecoder) count() int {
	v := d.uvarint()
	if v > maxBatchCount || v > uint64(len(d.b)) {
		d.fail("count out of range")
		return 0
	}
	return int(v)
}

func (d *batchDecoder) bytes(n int) []byte {
	if d.err != nil {
		return nil
	}
	if n > len(d.b) {
		d.fail("short")
		return nil
	}
	v := d.b[:n:n]
	d.b = d.b[n:]
	return v
}

func (d *batchDecoder) str() string {
	n := d.uvarint()
	if n > uint64(len(d.b)) {
		d.fail("string longer than the batch")
		return ""
	}
	return string(d.bytes(int(n)))
}

func (d *batchDecoder) arg() any {
	t := d.bytes(1)
	if d.err != nil {
		return nil
	}
	switch t[0] {
	case argNil:
		return nil
	case argInt:
		v, n := binary.Varint(d.b)
		if n <= 0 {
			d.fail("bad integer")
			return nil
		}
		d.b = d.b[n:]
		return v
	case argFloat:
		b := d.bytes(8)
		if d.err != nil {
			return nil
		}
		return math.Float64frombits(binary.LittleEndian.Uint64(b))
	case argBool:
		b := d.bytes(1)
		if d.err != nil {
			return nil
		}
		return b[0] != 0
	case argBytes:
		return []byte(d.str())
	case argString:
		return d.str()
	case argTime:
		s := d.str()
		if d.err != nil {
			return nil
		}
		var v time.Time
		if err := v.UnmarshalBinary([]byte(s)); err != nil {
			d.fail("bad time")
			return nil
		}
		return v
	default:
		d.fail(fmt.Sprintf("argument type %d", t[0]))
		return nil
	}
}
