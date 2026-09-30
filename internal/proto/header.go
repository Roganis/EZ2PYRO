// Package proto defines the linkdoctor wire formats: the 40-byte data packet
// header, the probe ping format and the JSON-lines control messages.
package proto

import (
	"encoding/binary"
	"errors"
)

const (
	// Magic identifies a linkdoctor data packet ("PW").
	Magic uint16 = 0x5057
	// ProbeMagic identifies a probe ping ("PR").
	ProbeMagic uint16 = 0x5052
	// Version is the wire protocol version.
	Version uint8 = 1

	// HeaderSize is the size of the data packet header in bytes.
	HeaderSize = 40
	// ProbeSize is the size of a probe packet in bytes.
	ProbeSize = 32

	// MinPacketSize and MaxPacketSize bound the UDP payload size.
	MinPacketSize = 200
	MaxPacketSize = 1472
)

// Header flags.
const (
	FlagLastPacket uint8 = 1 << 0 // last packet of the frame
	FlagProbe      uint8 = 1 << 1 // reserved: probe traffic on the data port
	FlagPunch      uint8 = 1 << 2 // receiver → sender hole-punch packet
)

var (
	ErrShort      = errors.New("proto: packet too short")
	ErrBadMagic   = errors.New("proto: bad magic")
	ErrBadVersion = errors.New("proto: unsupported version")
)

// Header is the little-endian data packet header.
type Header struct {
	Flags          uint8
	SessionID      uint32
	Seq            uint64
	FrameID        uint32
	PktIdx         uint16
	PktCount       uint16
	FrameSendStart int64 // sender monotonic clock, first packet of frame (ns)
	SendTS         int64 // sender monotonic clock, this packet (ns)
}

// Encode writes the header into b, which must be at least HeaderSize long.
func (h *Header) Encode(b []byte) {
	_ = b[HeaderSize-1]
	binary.LittleEndian.PutUint16(b[0:], Magic)
	b[2] = Version
	b[3] = h.Flags
	binary.LittleEndian.PutUint32(b[4:], h.SessionID)
	binary.LittleEndian.PutUint64(b[8:], h.Seq)
	binary.LittleEndian.PutUint32(b[16:], h.FrameID)
	binary.LittleEndian.PutUint16(b[20:], h.PktIdx)
	binary.LittleEndian.PutUint16(b[22:], h.PktCount)
	binary.LittleEndian.PutUint64(b[24:], uint64(h.FrameSendStart))
	binary.LittleEndian.PutUint64(b[32:], uint64(h.SendTS))
}

// Decode parses a header from b.
func (h *Header) Decode(b []byte) error {
	if len(b) < HeaderSize {
		return ErrShort
	}
	if binary.LittleEndian.Uint16(b[0:]) != Magic {
		return ErrBadMagic
	}
	if b[2] != Version {
		return ErrBadVersion
	}
	h.Flags = b[3]
	h.SessionID = binary.LittleEndian.Uint32(b[4:])
	h.Seq = binary.LittleEndian.Uint64(b[8:])
	h.FrameID = binary.LittleEndian.Uint32(b[16:])
	h.PktIdx = binary.LittleEndian.Uint16(b[20:])
	h.PktCount = binary.LittleEndian.Uint16(b[22:])
	h.FrameSendStart = int64(binary.LittleEndian.Uint64(b[24:]))
	h.SendTS = int64(binary.LittleEndian.Uint64(b[32:]))
	return nil
}

// Probe is a timestamped ping used for RTT and clock-offset estimation.
// T1 is the client send time, T2 the server receive time and T3 the server
// send time, each on the respective machine's monotonic clock.
type Probe struct {
	Reply bool
	Seq   uint32
	T1    int64
	T2    int64
	T3    int64
}

// Encode writes the probe into b, which must be at least ProbeSize long.
func (p *Probe) Encode(b []byte) {
	_ = b[ProbeSize-1]
	binary.LittleEndian.PutUint16(b[0:], ProbeMagic)
	b[2] = Version
	b[3] = 0
	if p.Reply {
		b[3] = 1
	}
	binary.LittleEndian.PutUint32(b[4:], p.Seq)
	binary.LittleEndian.PutUint64(b[8:], uint64(p.T1))
	binary.LittleEndian.PutUint64(b[16:], uint64(p.T2))
	binary.LittleEndian.PutUint64(b[24:], uint64(p.T3))
}

// Decode parses a probe from b.
func (p *Probe) Decode(b []byte) error {
	if len(b) < ProbeSize {
		return ErrShort
	}
	if binary.LittleEndian.Uint16(b[0:]) != ProbeMagic {
		return ErrBadMagic
	}
	if b[2] != Version {
		return ErrBadVersion
	}
	p.Reply = b[3] == 1
	p.Seq = binary.LittleEndian.Uint32(b[4:])
	p.T1 = int64(binary.LittleEndian.Uint64(b[8:]))
	p.T2 = int64(binary.LittleEndian.Uint64(b[16:]))
	p.T3 = int64(binary.LittleEndian.Uint64(b[24:]))
	return nil
}
