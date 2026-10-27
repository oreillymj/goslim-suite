package mdns

// A minimal DNS message encoder/decoder: just what DNS-SD needs (PTR, SRV, TXT
// and A records, one question per query).

import (
	"encoding/binary"
	"errors"
	"net"
	"strings"
)

const (
	typeA   = 1
	typePTR = 12
	typeTXT = 16
	typeSRV = 33
	typeANY = 255

	classIN    = 1
	cacheFlush = 0x8000
)

type question struct {
	name  string
	qtype uint16
}

type record struct {
	name  string
	rtype uint16
	ttl   uint32
	// by type:
	target string // PTR, SRV
	port   int    // SRV
	txt    []string
	ip     net.IP // A
}

type message struct {
	response  bool
	questions []question
	answers   []record
}

func appendName(b []byte, name string) ([]byte, error) {
	name = strings.TrimSuffix(name, ".")
	if name == "" {
		return append(b, 0), nil
	}
	for _, l := range strings.Split(name, ".") {
		if len(l) == 0 || len(l) > 63 {
			return nil, errors.New("mdns: bad label in " + name)
		}
		b = append(b, byte(len(l)))
		b = append(b, l...)
	}
	return append(b, 0), nil
}

func (m *message) pack() ([]byte, error) {
	b := make([]byte, 12, 512)
	if m.response {
		b[2] = 0x84 // QR + AA
	}
	binary.BigEndian.PutUint16(b[4:], uint16(len(m.questions)))
	binary.BigEndian.PutUint16(b[6:], uint16(len(m.answers)))
	var err error
	for _, q := range m.questions {
		if b, err = appendName(b, q.name); err != nil {
			return nil, err
		}
		b = binary.BigEndian.AppendUint16(b, q.qtype)
		b = binary.BigEndian.AppendUint16(b, classIN)
	}
	for _, r := range m.answers {
		if b, err = appendName(b, r.name); err != nil {
			return nil, err
		}
		class := uint16(classIN)
		if r.rtype != typePTR {
			class |= cacheFlush
		}
		b = binary.BigEndian.AppendUint16(b, r.rtype)
		b = binary.BigEndian.AppendUint16(b, class)
		b = binary.BigEndian.AppendUint32(b, r.ttl)
		lenAt := len(b)
		b = append(b, 0, 0)
		switch r.rtype {
		case typePTR:
			b, err = appendName(b, r.target)
		case typeSRV:
			b = binary.BigEndian.AppendUint16(b, 0) // priority
			b = binary.BigEndian.AppendUint16(b, 0) // weight
			b = binary.BigEndian.AppendUint16(b, uint16(r.port))
			b, err = appendName(b, r.target)
		case typeTXT:
			if len(r.txt) == 0 {
				b = append(b, 0)
			}
			for _, s := range r.txt {
				if len(s) > 255 {
					return nil, errors.New("mdns: TXT string too long")
				}
				b = append(b, byte(len(s)))
				b = append(b, s...)
			}
		case typeA:
			ip4 := r.ip.To4()
			if ip4 == nil {
				return nil, errors.New("mdns: not an IPv4 address")
			}
			b = append(b, ip4...)
		}
		if err != nil {
			return nil, err
		}
		binary.BigEndian.PutUint16(b[lenAt:], uint16(len(b)-lenAt-2))
	}
	return b, nil
}

// readName decodes a possibly compressed name starting at off and returns it
// with the offset after it in the original position.
func readName(msg []byte, off int) (string, int, error) {
	var labels []string
	next := -1
	for hops := 0; ; {
		if off >= len(msg) {
			return "", 0, errors.New("mdns: short name")
		}
		c := int(msg[off])
		switch {
		case c == 0:
			if next < 0 {
				next = off + 1
			}
			return strings.Join(labels, ".") + ".", next, nil
		case c&0xc0 == 0xc0:
			if off+1 >= len(msg) {
				return "", 0, errors.New("mdns: short pointer")
			}
			if next < 0 {
				next = off + 2
			}
			if hops++; hops > 20 {
				return "", 0, errors.New("mdns: pointer loop")
			}
			off = (c&0x3f)<<8 | int(msg[off+1])
		case c&0xc0 != 0:
			return "", 0, errors.New("mdns: bad label")
		default:
			if off+1+c > len(msg) {
				return "", 0, errors.New("mdns: short label")
			}
			labels = append(labels, string(msg[off+1:off+1+c]))
			off += 1 + c
		}
	}
}

func parse(msg []byte) (*message, error) {
	if len(msg) < 12 {
		return nil, errors.New("mdns: short message")
	}
	m := &message{response: msg[2]&0x80 != 0}
	qd := int(binary.BigEndian.Uint16(msg[4:]))
	an := int(binary.BigEndian.Uint16(msg[6:]))
	rest := int(binary.BigEndian.Uint16(msg[8:])) + int(binary.BigEndian.Uint16(msg[10:]))
	off := 12
	for i := 0; i < qd; i++ {
		name, n, err := readName(msg, off)
		if err != nil || n+4 > len(msg) {
			return nil, errors.New("mdns: bad question")
		}
		m.questions = append(m.questions, question{name, binary.BigEndian.Uint16(msg[n:])})
		off = n + 4
	}
	for i := 0; i < an+rest; i++ {
		name, n, err := readName(msg, off)
		if err != nil || n+10 > len(msg) {
			return nil, errors.New("mdns: bad record")
		}
		r := record{name: name, rtype: binary.BigEndian.Uint16(msg[n:]), ttl: binary.BigEndian.Uint32(msg[n+4:])}
		rdlen := int(binary.BigEndian.Uint16(msg[n+8:]))
		rd := n + 10
		if rd+rdlen > len(msg) {
			return nil, errors.New("mdns: bad record length")
		}
		switch r.rtype {
		case typePTR:
			r.target, _, err = readName(msg, rd)
		case typeSRV:
			if rdlen < 7 {
				err = errors.New("mdns: short SRV")
				break
			}
			r.port = int(binary.BigEndian.Uint16(msg[rd+4:]))
			r.target, _, err = readName(msg, rd+6)
		case typeTXT:
			for p := rd; p < rd+rdlen; {
				l := int(msg[p])
				if p+1+l > rd+rdlen {
					break
				}
				if l > 0 {
					r.txt = append(r.txt, string(msg[p+1:p+1+l]))
				}
				p += 1 + l
			}
		case typeA:
			if rdlen == 4 {
				r.ip = net.IP(append([]byte(nil), msg[rd:rd+4]...))
			}
		}
		if err != nil {
			return nil, err
		}
		m.answers = append(m.answers, r) // authority/additional records are as good as answers for us
		off = rd + rdlen
	}
	return m, nil
}
