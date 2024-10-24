// Package datapath implements linux data path for PPPoE/PPP;
//
//	TODO: currently datapath does NOT do following:
//		- create default route with nexthop as the TUN interface
//		- apply DNS server address
package datapath

import (
	"context"
	"fmt"
	"github.com/gandalfast/zouppp/lcp"
	"github.com/rs/zerolog"
	"github.com/songgao/water"
	"github.com/vishvananda/netlink"
	"net"
)

// TUNInterface is the TUN interface for a opened PPP session
type TUNInterface struct {
	logger                 *zerolog.Logger
	netInterface           *water.Interface
	netLink                netlink.Link
	sendChan               chan []byte
	v4recvChan, v6recvChan chan []byte
}

const (
	// IPv4 header size: 20 bytes (min)
	// IPv6 header size: 40 bytes
	_minimumFrameSize = 20 //ipv4 header

	// _defaultMaxFrameSize is the default max PPP frame size could be received from the TUN interface
	_defaultMaxFrameSize = 1500
)

// NewTUNIf creates a new TUN interface supporting PPP protocol.
// The interface name must be specified in the parameters, and all the assigned addresses
// are copied into the TUN interface.
// MTU value is the value of peerMRU parameter.
func NewTUNIf(ctx context.Context, pppproto *lcp.PPP, name string, assignedAddrs []net.IP, peerMRU uint16) (tun *TUNInterface, err error) {
	tun = new(TUNInterface)
	cfg := water.Config{
		DeviceType: water.TUN,
		PlatformSpecificParams: water.PlatformSpecificParams{
			Name: name,
		},
	}

	// Create TUN interface
	tun.netInterface, err = water.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create TUN interface %v, %w", cfg.Name, err)
	}

	// Enable network link
	tun.netLink, _ = netlink.LinkByName(cfg.Name)
	if err := netlink.LinkSetUp(tun.netLink); err != nil {
		return nil, fmt.Errorf("failed to bring the TUN interface %v up, %w", cfg.Name, err)
	}

	// Add remote address
	for _, addr := range assignedAddrs {
		if addr == nil {
			continue
		}
		if !addr.IsUnspecified() && len(addr) > 0 {
			var addressMask string
			if addr.To4() != nil {
				addressMask = "/32"
				tun.sendChan, tun.v4recvChan = pppproto.Register(lcp.ProtoIPv4)
			} else {
				addressMask = "/128"
				tun.sendChan, tun.v6recvChan = pppproto.Register(lcp.ProtoIPv6)
			}

			addrString := addr.String() + addressMask
			netAddr, err := netlink.ParseAddr(addrString)
			if err != nil {
				return nil, fmt.Errorf("failed to parse %v as IP addr, %w", addrString, err)
			}

			// Add default remote route to the interface
			err = netlink.AddrAdd(tun.netLink, netAddr)
			if err != nil {
				return nil, fmt.Errorf("failed to add addr %v, %w", addrString, err)
			}
		}
	}

	// Adjust MTU based on PPP peer's MRU
	mtu := int(peerMRU)
	if mtu < 1280 {
		mtu = 1280
	}
	_ = netlink.LinkSetMTU(tun.netLink, mtu)

	logger := pppproto.GetLogger().With().Str("Name", "datapath").Logger()
	tun.logger = &logger
	go tun.send(ctx)
	go tun.recv(ctx)
	return tun, nil
}

// send pkt to outside network
func (tif *TUNInterface) send(ctx context.Context) {
	for {
		// Read IPv4 / IPv6 packet to send from TUN interface
		buf := make([]byte, _defaultMaxFrameSize)
		n, err := tif.netInterface.Read(buf)
		if err != nil {
			tif.logger.Error().Err(err).Msg("failed to read net interface packet")
			return
		}
		buf = buf[:n]

		// Check if context is still valid
		select {
		case <-ctx.Done():
			tif.logger.Info().Msg("send routine stopped")
			_ = tif.netInterface.Close()
			return
		default:
		}

		// Packet is too small, discard
		if n < _minimumFrameSize {
			continue
		}

		// Check Version value from IPv4 / IPv6 header, and encapsulate
		// into PPP accordingly
		switch buf[0] >> 4 {
		case 4:
			pkt, err := lcp.NewPPPPkt(lcp.NewStaticSerializer(buf[:n]), lcp.ProtoIPv4).Serialize()
			if err == nil {
				tif.sendChan <- pkt
			}
		case 6:
			pkt, err := lcp.NewPPPPkt(lcp.NewStaticSerializer(buf[:n]), lcp.ProtoIPv6).Serialize()
			if err == nil {
				tif.sendChan <- pkt
			}
		default:
			tif.logger.Info().Msg("unable to send packet with unknown IP version")
			continue
		}
	}
}

// recv gets packet from outside network
func (tif *TUNInterface) recv(ctx context.Context) {
	for {
		var pktbytes []byte

		select {
		case <-ctx.Done():
			tif.logger.Info().Msg("recv routine stopped")
			return
		case pktbytes = <-tif.v4recvChan:
			// Save data into pktbytes
		case pktbytes = <-tif.v6recvChan:
			// Save data into pktbytes
		}

		if _, err := tif.netInterface.Write(pktbytes); err != nil {
			tif.logger.Error().Err(err).Msg("failed to send data to TUN interface")
			return
		}
	}
}
