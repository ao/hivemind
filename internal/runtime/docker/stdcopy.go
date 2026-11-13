package docker

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Stream types in the 8-byte frame header, as the engine writes them for a
// container without a TTY.
const (
	streamStdin  = 0
	streamStdout = 1
	streamStderr = 2
	streamSystem = 3
)

// StdCopy splits a multiplexed log stream into stdout and stderr. Each frame
// is an 8-byte header followed by a payload: one byte of stream type (0 and 1
// go to stdout, 2 to stderr, 3 is discarded), three reserved bytes, and a
// big-endian uint32 payload length.
//
// It returns nil when src ends on a frame boundary, io.ErrUnexpectedEOF when
// it ends inside a frame, and an error for an unknown stream type. The payload
// is copied in pieces, so a corrupt length cannot make it allocate memory.
func StdCopy(stdout, stderr io.Writer, src io.Reader) error {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	var hdr [8]byte
	for {
		_, err := io.ReadFull(src, hdr[:])
		if err == io.EOF {
			return nil
		}
		if err != nil {
			// io.ReadFull returns ErrUnexpectedEOF for a partial header.
			return err
		}
		var dst io.Writer
		switch hdr[0] {
		case streamStdin, streamStdout:
			dst = stdout
		case streamStderr:
			dst = stderr
		case streamSystem:
			dst = io.Discard
		default:
			return fmt.Errorf("log stream: unknown stream type %d", hdr[0])
		}
		size := int64(binary.BigEndian.Uint32(hdr[4:8]))
		n, err := io.CopyN(dst, src, size)
		if err != nil {
			if err == io.EOF {
				return io.ErrUnexpectedEOF
			}
			return err
		}
		if n != size {
			return io.ErrUnexpectedEOF
		}
	}
}
