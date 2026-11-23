package phobos

// move.go - clc_move (player input), ported from client/cl_input.c
// (CL_SendCmd), qcommon/common.c (MSG_WriteDeltaUsercmd,
// COM_BlockSequenceCRCByte) and qcommon/crc.c (CRC_Block).
//
// The server drops a clc_move whose checksum byte doesn't match, so the
// checksum has to be bit-exact.

// usercmd_t delta bits
const (
	cmAngle1  = 1 << 0
	cmAngle2  = 1 << 1
	cmAngle3  = 1 << 2
	cmForward = 1 << 3
	cmSide    = 1 << 4
	cmUp      = 1 << 5
	cmButtons = 1 << 6
	cmImpulse = 1 << 7
)

// usercmd buttons (q_shared.h)
const (
	buttonAttack = 1
	buttonUse    = 2

	ButtonAttack = buttonAttack // exported for cmd/morsebot
)

type usercmd struct {
	msec       byte
	buttons    byte
	angles     [3]int16
	forward    int16
	side       int16
	up         int16
	impulse    byte
	lightlevel byte
}

// writeDeltaUsercmd mirrors MSG_WriteDeltaUsercmd.
func writeDeltaUsercmd(w *msgWriter, from, cmd *usercmd) {
	bits := byte(0)
	for i, b := range []byte{cmAngle1, cmAngle2, cmAngle3} {
		if cmd.angles[i] != from.angles[i] {
			bits |= b
		}
	}
	if cmd.forward != from.forward {
		bits |= cmForward
	}
	if cmd.side != from.side {
		bits |= cmSide
	}
	if cmd.up != from.up {
		bits |= cmUp
	}
	if cmd.buttons != from.buttons {
		bits |= cmButtons
	}
	if cmd.impulse != from.impulse {
		bits |= cmImpulse
	}
	w.Byte(bits)
	for i, b := range []byte{cmAngle1, cmAngle2, cmAngle3} {
		if bits&b != 0 {
			w.Short(uint16(cmd.angles[i]))
		}
	}
	if bits&cmForward != 0 {
		w.Short(uint16(cmd.forward))
	}
	if bits&cmSide != 0 {
		w.Short(uint16(cmd.side))
	}
	if bits&cmUp != 0 {
		w.Short(uint16(cmd.up))
	}
	if bits&cmButtons != 0 {
		w.Byte(cmd.buttons)
	}
	if bits&cmImpulse != 0 {
		w.Byte(cmd.impulse)
	}
	w.Byte(cmd.msec)
	w.Byte(cmd.lightlevel)
}

// crcBlock mirrors CRC_Block: CRC-16/CCITT, init 0xffff, no final xor.
func crcBlock(data []byte) uint16 {
	crc := uint16(0xffff)
	for _, b := range data {
		x := (crc >> 8) ^ uint16(b)
		// table entry computed on the fly (crctable[] in crc.c, poly 0x1021)
		t := x << 8
		for i := 0; i < 8; i++ {
			if t&0x8000 != 0 {
				t = t<<1 ^ 0x1021
			} else {
				t <<= 1
			}
		}
		crc = crc<<8 ^ t
	}
	return crc
}

// blockSequenceCRCByte mirrors COM_BlockSequenceCRCByte.
func blockSequenceCRCByte(base []byte, sequence uint32) byte {
	p := chktbl[sequence%(uint32(len(chktbl))-4):]
	if len(base) > 60 {
		base = base[:60]
	}
	chkb := make([]byte, 0, 64)
	chkb = append(chkb, base...)
	chkb = append(chkb, p[0], p[1], p[2], p[3])

	crc := crcBlock(chkb)
	x := 0
	for _, b := range chkb {
		x += int(b)
	}
	return byte((int(crc) ^ x) & 0xff)
}

// buildMove builds a clc_move for the packet that will go out with the given
// outgoing sequence number. As in CL_SendCmd it carries the last three
// commands so the server can recover from a couple of dropped packets.
// lastFrame -1 asks the server for uncompressed (non-delta) frames.
func buildMove(sequence uint32, lastFrame int32, cmds [3]usercmd) []byte {
	var w msgWriter
	w.Byte(clcMove)
	checksumIndex := w.Len()
	w.Byte(0)
	w.Long(uint32(lastFrame))
	var null usercmd
	writeDeltaUsercmd(&w, &null, &cmds[0])
	writeDeltaUsercmd(&w, &cmds[0], &cmds[1])
	writeDeltaUsercmd(&w, &cmds[1], &cmds[2])
	w.buf[checksumIndex] = blockSequenceCRCByte(w.buf[checksumIndex+1:], sequence)
	return w.buf
}
