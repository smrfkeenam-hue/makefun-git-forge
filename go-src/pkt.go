// pkt-line framing for the git smart-HTTP protocol (gitprotocol-pack /
// gitprotocol-v2).
//
// A data packet is 4 lowercase-hex ASCII bytes giving the total length
// including the header, then the payload. Control packets: flush "0000", delim
// "0001", response-end "0002". The largest packet is 65520 bytes (git's
// LARGE_PACKET_MAX, header included), so a payload is at most 65516.
// Side-band multiplexing (side-band-64k) prefixes each packet's payload with a
// band byte: 1 = data, 2 = progress, 3 = fatal error (this server never sends
// band 3).

package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// The largest pkt-line, header included.
const pktLineMax = 65520

// The largest payload a pkt-line carries.
const pktMaxPayload = pktLineMax - 4

// The flush packet, "0000".
var flushPkt = []byte("0000")

// The delim packet, "0001".
var delimPkt = []byte("0001")

// The response-end packet, "0002".
var responseEndPkt = []byte("0002")

// PacketKind distinguishes data packets from control markers.
type packetKind int

const (
	pktData packetKind = iota
	pktFlush
	pktDelim
	pktResponseEnd
)

// Packet is one decoded packet: a data payload, or a control marker.
type packet struct {
	kind    packetKind
	payload []byte // data packets only; a view into the decoded buffer
}

// pktLine encodes one payload as a pkt-line. It errors when the payload does
// not fit.
func pktLine(payload []byte) ([]byte, error) {
	if len(payload) > pktMaxPayload {
		return nil, fmt.Errorf("pkt-line payload of %d bytes exceeds %d", len(payload), pktMaxPayload)
	}
	out := make([]byte, 4+len(payload))
	hexLen := strconv.FormatInt(int64(len(payload)+4), 16)
	for len(hexLen) < 4 {
		hexLen = "0" + hexLen
	}
	copy(out, hexLen)
	copy(out[4:], payload)
	return out, nil
}

// pktLineString encodes one string payload as a pkt-line.
func pktLineString(payload string) ([]byte, error) {
	return pktLine([]byte(payload))
}

// mustPktLine encodes one payload, panicking when it does not fit (all call
// sites build small control payloads).
func mustPktLine(payload []byte) []byte {
	b, err := pktLine(payload)
	if err != nil {
		panic(err)
	}
	return b
}

// mustPktLineString encodes one string payload, panicking when it does not fit.
func mustPktLineString(payload string) []byte {
	return mustPktLine([]byte(payload))
}

// pktLines encodes several payloads, each its own pkt-line, concatenated.
func pktLines(payloads ...[]byte) []byte {
	var out []byte
	for _, p := range payloads {
		out = append(out, mustPktLine(p)...)
	}
	return out
}

// pktLinesStrings encodes several string payloads, each its own pkt-line.
func pktLinesStrings(payloads ...string) []byte {
	var out []byte
	for _, p := range payloads {
		out = append(out, mustPktLineString(p)...)
	}
	return out
}

// decodePktLines decodes a whole buffer of pkt-lines. With allowTrailing it
// returns the bytes after the last complete packet (the receive-pack request
// carries a raw packfile after its flush). A malformed header, an over-long
// packet or a truncated tail errors.
func decodePktLines(b []byte, allowTrailing bool) ([]packet, []byte, error) {
	var packets []packet
	offset := 0
	for offset < len(b) {
		if offset+4 > len(b) {
			return nil, nil, errors.New("pktline-truncated: short header at end of buffer")
		}
		length := 0
		valid := true
		for i := 0; i < 4; i++ {
			c := b[offset+i]
			var digit int
			switch {
			case c >= '0' && c <= '9':
				digit = int(c - '0')
			case c >= 'a' && c <= 'f':
				digit = int(c-'a') + 10
			default:
				digit = -1
			}
			if digit == -1 {
				valid = false
				break
			}
			length = length*16 + digit
		}
		if !valid {
			if allowTrailing {
				break
			}
			return nil, nil, fmt.Errorf("pktline-header: bad length %q", string(b[offset:offset+4]))
		}
		switch length {
		case 0:
			packets = append(packets, packet{kind: pktFlush})
			offset += 4
			continue
		case 1:
			packets = append(packets, packet{kind: pktDelim})
			offset += 4
			continue
		case 2:
			packets = append(packets, packet{kind: pktResponseEnd})
			offset += 4
			continue
		case 3:
			return nil, nil, errors.New("pktline-header: length 0003 is not a packet")
		}
		if length > pktLineMax {
			return nil, nil, fmt.Errorf("pktline-too-long: %d bytes", length)
		}
		if offset+length > len(b) {
			return nil, nil, errors.New("pktline-truncated: packet overruns the buffer")
		}
		// A view, not a copy: no caller mutates a payload.
		packets = append(packets, packet{kind: pktData, payload: b[offset+4 : offset+length]})
		offset += length
	}
	trailing := b[offset:]
	if !allowTrailing && len(trailing) > 0 {
		return nil, nil, errors.New("pktline-after-end: trailing bytes after the packets")
	}
	return packets, trailing, nil
}

// packetLines returns the data payloads of decoded packets as UTF-8 strings,
// control packets dropped.
func packetLines(packets []packet) []string {
	var lines []string
	for _, p := range packets {
		if p.kind == pktData {
			lines = append(lines, string(p.payload))
		}
	}
	return lines
}

// sideband frames arbitrary bytes as side-band packets of the given band, each
// at most pktMaxPayload-1 bytes of payload (one byte is the band marker).
func sideband(data []byte, band byte) [][]byte {
	var chunks [][]byte
	max := pktMaxPayload - 1
	for offset := 0; offset < len(data); offset += max {
		end := offset + max
		if end > len(data) {
			end = len(data)
		}
		framed := make([]byte, 1+end-offset)
		framed[0] = band
		copy(framed[1:], data[offset:end])
		chunks = append(chunks, mustPktLine(framed))
	}
	return chunks
}

// parseCapabilities parses a capability list ("key=value key2 ...") into a
// map; a bare key maps to ''.
func parseCapabilities(text string) map[string]string {
	caps := map[string]string{}
	for _, token := range strings.Split(text, " ") {
		if token == "" {
			continue
		}
		if at := strings.Index(token, "="); at == -1 {
			caps[token] = ""
		} else {
			caps[token[:at]] = token[at+1:]
		}
	}
	return caps
}

// renderCapabilities renders a capability list back to its wire form, keys in
// the given order.
func renderCapabilities(ordered [][2]string) string {
	parts := make([]string, 0, len(ordered))
	for _, kv := range ordered {
		if kv[1] == "" {
			parts = append(parts, kv[0])
		} else {
			parts = append(parts, kv[0]+"="+kv[1])
		}
	}
	return strings.Join(parts, " ")
}
