package phobos

// msg.go - byte-level message reading/writing, mirroring MSG_* in
// qcommon/common.c of the Quake 2 GPL source. Everything on the wire is
// little-endian.

import (
	"encoding/binary"
)

// ---------------------------------------------------------------- writing

type msgWriter struct {
	buf []byte
}

func (w *msgWriter) Byte(v byte)    { w.buf = append(w.buf, v) }
func (w *msgWriter) Short(v uint16) { w.buf = binary.LittleEndian.AppendUint16(w.buf, v) }
func (w *msgWriter) Long(v uint32)  { w.buf = binary.LittleEndian.AppendUint32(w.buf, v) }
func (w *msgWriter) Bytes(b []byte) { w.buf = append(w.buf, b...) }

// String writes a NUL-terminated string (MSG_WriteString).
func (w *msgWriter) String(s string) {
	w.buf = append(w.buf, s...)
	w.buf = append(w.buf, 0)
}

func (w *msgWriter) Len() int { return len(w.buf) }

// ---------------------------------------------------------------- reading

// msgReader behaves like Quake 2's sizebuf reader: reading past the end
// does not crash, it just sets the sticky 'bad' flag (readcount > cursize).
type msgReader struct {
	data []byte
	pos  int
	bad  bool
}

func newReader(b []byte) *msgReader { return &msgReader{data: b} }

func (r *msgReader) need(n int) bool {
	if r.pos+n > len(r.data) {
		r.bad = true
		r.pos = len(r.data)
		return false
	}
	return true
}

// Byte returns -1 at end of message, exactly like MSG_ReadByte.
func (r *msgReader) Byte() int {
	if !r.need(1) {
		return -1
	}
	v := r.data[r.pos]
	r.pos++
	return int(v)
}

func (r *msgReader) Char() int {
	if !r.need(1) {
		return -1
	}
	v := int8(r.data[r.pos])
	r.pos++
	return int(v)
}

func (r *msgReader) Short() int {
	if !r.need(2) {
		return -1
	}
	v := int16(binary.LittleEndian.Uint16(r.data[r.pos:]))
	r.pos += 2
	return int(v)
}

func (r *msgReader) Long() int32 {
	if !r.need(4) {
		return -1
	}
	v := int32(binary.LittleEndian.Uint32(r.data[r.pos:]))
	r.pos += 4
	return v
}

func (r *msgReader) ULong() uint32 { return uint32(r.Long()) }

// String reads up to a NUL (MSG_ReadString).
func (r *msgReader) String() string {
	start := r.pos
	for r.pos < len(r.data) {
		if r.data[r.pos] == 0 {
			s := string(r.data[start:r.pos])
			r.pos++
			return s
		}
		r.pos++
	}
	// ran off the end without a terminator
	r.bad = true
	return string(r.data[start:])
}

func (r *msgReader) Skip(n int) {
	if r.need(n) {
		r.pos += n
	}
}

// Q2 helpers: a coord is a short (1/8 unit), a pos is three coords,
// a dir is a single byte index into the anorms table.
func (r *msgReader) Pos() { r.Skip(6) }
func (r *msgReader) Dir() { r.Skip(1) }

// ---------------------------------------------------------------- text

// q2Text converts Quake 2 "high-bit" text (used for green/bold chat and
// console characters) into plain printable ASCII.
func q2Text(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i] & 0x7f
		switch {
		case c == '\n' || c == '\t':
			out = append(out, c)
		case c < 32:
			// the Q2 conchars font maps low codes to symbols; just skip them
		default:
			out = append(out, c)
		}
	}
	return string(out)
}
