package util

import (
	"log"
	"net"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
)

// CreateUDPResponse pack ip udp package, for tun device
func CreateUDPResponse(SrcIP net.IP, SrcPort uint16, DstIP net.IP, DstPort uint16, pkt []byte) []byte {
	ip := &layers.IPv4{
		SrcIP:    SrcIP,
		DstIP:    DstIP,
		Protocol: layers.IPProtocolUDP,
		Version:  uint8(4),
		IHL:      uint8(5),
		TTL:      uint8(64),
	}
	udp := &layers.UDP{SrcPort: layers.UDPPort(SrcPort), DstPort: layers.UDPPort(DstPort)}
	if err := udp.SetNetworkLayerForChecksum(ip); err != nil {
		log.Println("SetNetworkLayerForChecksum failed", err)
		return nil
	}
	buf := gopacket.NewSerializeBuffer()
	opts := gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}
	gopacket.SerializeLayers(buf, opts,
		ip,
		udp,
		gopacket.Payload(pkt),
	)

	packetData := buf.Bytes()
	return packetData
}
