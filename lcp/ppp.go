package lcp

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/gandalfast/zouppp/etherconn"
	"github.com/rs/zerolog"
	"net"
	"sync"
	"time"
)

// PPPPacket is the PPP packet
type PPPPacket struct {
	Proto   PPPProtocolNumber
	Payload Serializer
}

// Serialize into bytes, without copying, and no padding
func (pppPkt *PPPPacket) Serialize() ([]byte, error) {
	buf := make([]byte, 2)
	binary.BigEndian.PutUint16(buf, uint16(pppPkt.Proto))
	body, err := pppPkt.Payload.Serialize()
	if err != nil {
		return nil, err
	}
	return append(buf, body...), nil
}

// Parse buf into PPPPacket
func (pppPkt *PPPPacket) Parse(buf []byte) error {
	if len(buf) <= 2 {
		return fmt.Errorf("invalid PPP packet length %d", len(buf))
	}
	pppPkt.Proto = PPPProtocolNumber(binary.BigEndian.Uint16(buf[:2]))
	pppPkt.Payload = NewStaticSerializer(buf[2:])
	return nil
}

// NewPPPPkt return a new PPPPacket with proto and payload
func NewPPPPkt(data Serializer, proto PPPProtocolNumber) *PPPPacket {
	r := new(PPPPacket)
	r.Payload = data
	r.Proto = proto
	return r
}

// PPP is the PPP protcol, other protocol like IPv4/IPv6/LCP/IPCP/IPv6CP runs over it
type PPP struct {
	relayChanList     map[PPPProtocolNumber]chan []byte
	sendChan          chan []byte
	relayChanListLock *sync.RWMutex
	conn              net.PacketConn
	logger            *zerolog.Logger
	reqID             uint8 //used by send project-reject
}

// NewPPP creates a new PPP protocol instance, using conn as underlying transport, l as logger;
func NewPPP(ctx context.Context, conn net.PacketConn, l *zerolog.Logger) *PPP {
	r := new(PPP)
	r.relayChanList = make(map[PPPProtocolNumber]chan []byte)
	r.relayChanListLock = new(sync.RWMutex)
	r.conn = conn
	r.sendChan = make(chan []byte, sendCHanDepth)
	r.logger = l
	go r.recv(ctx)
	go r.send(ctx)
	return r
}

const (
	relayChanDepth = 128
	sendCHanDepth  = 128
	// MaxPPPMsgSize specifies max length of a received PPP pkt
	MaxPPPMsgSize = 1500
)

// Register a new protocol to run over ppp;
// return two byte slice channels, send could use to send pkt over ppp, recv is used to recv pkt from ppp
func (ppp *PPP) Register(p PPPProtocolNumber) (send, recv chan []byte) {
	ch := make(chan []byte, relayChanDepth)
	ppp.relayChanListLock.Lock()
	ppp.relayChanList[p] = ch
	ppp.relayChanListLock.Unlock()
	send = ppp.sendChan
	recv = ch
	return
}

// Un-register the protocol;
func (ppp *PPP) UnRegister(p PPPProtocolNumber) {
	ppp.relayChanListLock.Lock()
	close(ppp.relayChanList[p])
	delete(ppp.relayChanList, p)
	ppp.relayChanListLock.Unlock()
}

// GetLogger return the logger
func (ppp *PPP) GetLogger() *zerolog.Logger {
	return ppp.logger
}

func (ppp *PPP) send(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			ppp.logger.Info().Msg("ppp send routined stopped")
			return
		case b := <-ppp.sendChan:
			if _, err := ppp.conn.WriteTo(b, nil); err != nil {
				ppp.logger.Warn().Err(err).Msg("failed to send pkt")
			}
		}
	}
}

func (ppp *PPP) recv(ctx context.Context) {
	for {
		buf := make([]byte, MaxPPPMsgSize)
		ppp.conn.SetReadDeadline(time.Now().Add(readTimeout))
		n, _, err := ppp.conn.ReadFrom(buf)

		if err != nil && !errors.Is(err, etherconn.ErrTimeOut) {
			ppp.logger.Error().Err(err).Msg("failed to recv")
			return
		} else if err != nil {
			select {
			case <-ctx.Done():
				ppp.logger.Info().Msg("ppp recv routined stopped")
				return
			default:
			}
			continue
		}

		go ppp.relay(buf[:n])
	}
}

// b is the recvd unknown protocol pkt
func (ppp *PPP) sendProtocolRejct(b []byte) {
	proto := make([]byte, 2)
	copy(proto, b[:2])
	switch PPPProtocolNumber(binary.BigEndian.Uint16(proto)) {
	case ProtoCHAP, ProtoIPCP, ProtoLCP, ProtoPAP, ProtoIPv6CP, ProtoIPv4, ProtoIPv6:
		return
	}
	pkt := NewPkt(ProtoLCP)
	pkt.Code = CodeProtocolReject
	ppp.reqID++
	pkt.ID = ppp.reqID
	pkt.Payload = append(proto, b...)
	pktbytes, err := NewPPPPkt(pkt, ProtoLCP).Serialize()
	if err == nil {
		ppp.sendChan <- pktbytes
	}
	ppp.logger.Debug().Any("packet", pkt).Msg("send protocol reject")
}

func (ppp *PPP) relay(buf []byte) {
	if len(buf) <= 2 {
		return
	}
	ppp.relayChanListLock.RLock()
	defer ppp.relayChanListLock.RUnlock()
	if ch, ok := ppp.relayChanList[PPPProtocolNumber(binary.BigEndian.Uint16(buf[:2]))]; ok {
		ch <- buf[2:]
	}
	go ppp.sendProtocolRejct(buf)
}
