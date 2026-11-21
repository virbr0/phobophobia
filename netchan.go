package phobos

// netchan.go - a port of qcommon/net_chan.c (client side).
//
// Every in-game packet starts with:
//
//	uint32  sequence      (bit 31 set = packet carries a reliable message)
//	uint32  ack sequence  (bit 31     = last reliable sequence bit received)
//	uint16  qport         (client -> server only)
//
// Only one reliable message is ever "in flight". It is resent until the
// other side acknowledges it by echoing our reliable_sequence bit. While it
// is in flight, new reliable data accumulates in 'message' and is promoted
// to the reliable buffer once the previous one has been acked.

import (
	"encoding/binary"
	"fmt"
	"net"
	"time"
)

const (
	maxMsgLen       = 1400 // MAX_MSGLEN
	packetHeader    = 10   // 4 + 4 + 2
	oobMarker       = 0xffffffff
	protocolVersion = 34
)

type netchan struct {
	conn     *net.UDPConn
	qport    uint16
	isServer bool         // server side: no qport sent, one is read
	to       *net.UDPAddr // destination when conn is unconnected (server)

	incomingSequence             uint32
	incomingAcknowledged         uint32
	incomingReliableAcknowledged uint32 // single bit
	incomingReliableSequence     uint32 // single bit

	outgoingSequence     uint32
	reliableSequence     uint32 // single bit
	lastReliableSequence uint32

	message     msgWriter // pending reliable data, not yet in flight
	reliableBuf []byte    // reliable data currently in flight

	lastSent     time.Time
	lastReceived time.Time

	dropped int
}

func newNetchan(conn *net.UDPConn, qport uint16) *netchan {
	return &netchan{
		conn:             conn,
		qport:            qport,
		outgoingSequence: 1, // Netchan_Setup
		lastReceived:     time.Now(),
	}
}

// sendOOB sends a connectionless "out of band" packet: 0xFFFFFFFF + text.
func sendOOB(conn *net.UDPConn, text string) error {
	b := make([]byte, 4, 4+len(text))
	binary.LittleEndian.PutUint32(b, oobMarker)
	b = append(b, text...)
	_, err := conn.Write(b)
	return err
}

// needReliable mirrors Netchan_NeedReliable.
func (c *netchan) needReliable() bool {
	// the remote side dropped the last reliable message: resend it
	if c.incomingAcknowledged > c.lastReliableSequence &&
		c.incomingReliableAcknowledged != c.reliableSequence {
		return true
	}
	// nothing in flight and something waiting: send it
	return len(c.reliableBuf) == 0 && c.message.Len() > 0
}

// QueueReliable appends a reliable command (e.g. a clc_stringcmd).
func (c *netchan) QueueReliable(b []byte) error {
	if c.message.Len()+len(b) > maxMsgLen-packetHeader {
		return fmt.Errorf("reliable message overflow")
	}
	c.message.Bytes(b)
	return nil
}

// Transmit mirrors Netchan_Transmit. 'unreliable' may be nil.
func (c *netchan) Transmit(unreliable []byte) error {
	sendReliable := c.needReliable()

	if len(c.reliableBuf) == 0 && c.message.Len() > 0 {
		c.reliableBuf = append([]byte(nil), c.message.buf...)
		c.message.buf = c.message.buf[:0]
		c.reliableSequence ^= 1
	}

	var w msgWriter
	w1 := c.outgoingSequence &^ (1 << 31)
	if sendReliable {
		w1 |= 1 << 31
	}
	w2 := (c.incomingSequence &^ (1 << 31)) | (c.incomingReliableSequence << 31)

	c.outgoingSequence++
	c.lastSent = time.Now()

	w.Long(w1)
	w.Long(w2)
	if !c.isServer {
		w.Short(c.qport)
	}

	if sendReliable {
		w.Bytes(c.reliableBuf)
		c.lastReliableSequence = c.outgoingSequence
	}
	if maxMsgLen-w.Len() >= len(unreliable) {
		w.Bytes(unreliable)
	}

	var err error
	if c.to != nil {
		_, err = c.conn.WriteToUDP(w.buf, c.to)
	} else {
		_, err = c.conn.Write(w.buf)
	}
	return err
}

// Process mirrors Netchan_Process. It returns the payload (with the header
// stripped) and false if the packet is stale/duplicated and must be ignored.
func (c *netchan) Process(pkt []byte) ([]byte, bool) {
	if len(pkt) < 8 {
		return nil, false
	}
	sequence := binary.LittleEndian.Uint32(pkt[0:])
	sequenceAck := binary.LittleEndian.Uint32(pkt[4:])

	reliableMessage := sequence >> 31
	reliableAck := sequenceAck >> 31
	sequence &^= 1 << 31
	sequenceAck &^= 1 << 31

	// discard stale or duplicated packets
	if sequence <= c.incomingSequence {
		return nil, false
	}

	c.dropped = int(sequence - (c.incomingSequence + 1))

	// our in-flight reliable message has been received
	if reliableAck == c.reliableSequence {
		c.reliableBuf = c.reliableBuf[:0]
	}

	c.incomingSequence = sequence
	c.incomingAcknowledged = sequenceAck
	c.incomingReliableAcknowledged = reliableAck
	if reliableMessage != 0 {
		c.incomingReliableSequence ^= 1
	}
	c.lastReceived = time.Now()
	if c.isServer {
		if len(pkt) < 10 {
			return nil, false
		}
		return pkt[10:], true
	}
	return pkt[8:], true
}
