// Copyright 2022 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tcp

import (
	"fmt"
	"os"
	"testing"

	"gvisor.dev/gvisor/pkg/refs"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/waiter"
)

func TestMain(m *testing.M) {
	refs.SetLeakMode(refs.LeaksPanic)
	code := m.Run()
	refs.DoLeakCheck()
	os.Exit(code)
}

// A restored connect reinitializes route-dependent GSO without a handshake.
func TestRestoreConnectGSO(t *testing.T) {
	for _, supported := range []stack.SupportedGSO{stack.GSONotSupported, stack.GVisorGSOSupported, stack.HostGSOSupported} {
		for _, savedGSO := range []bool{false, true} {
			for _, mtu := range []uint32{1500, 1200} {
				t.Run(fmt.Sprintf("gso=%d/saved=%t/mtu=%d", supported, savedGSO, mtu), func(t *testing.T) {
					network := stack.New(stack.Options{NetworkProtocols: []stack.NetworkProtocolFactory{ipv4.NewProtocol}, TransportProtocols: []stack.TransportProtocolFactory{NewProtocol}})
					defer network.Destroy()
					link := channel.New(1, mtu, "")
					link.SupportedGSOKind = supported
					if err := network.CreateNIC(1, link); err != nil {
						t.Fatal(err)
					}
					local := tcpip.AddrFrom4([4]byte{10, 0, 0, 1})
					remote := tcpip.AddrFrom4([4]byte{10, 0, 0, 2})
					if err := network.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: header.IPv4ProtocolNumber, AddressWithPrefix: tcpip.AddressWithPrefix{Address: local, PrefixLen: 24}}, stack.AddressProperties{}); err != nil {
						t.Fatal(err)
					}
					network.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: 1}})
					ep := newEndpoint(network, network.TransportProtocolInstance(ProtocolNumber).(*protocol), header.IPv4ProtocolNumber, &waiter.Queue{})
					ep.mu.Lock()
					defer ep.mu.Unlock()
					ep.snd = &sender{ep: ep, gso: savedGSO, TCPSenderState: TCPSenderState{MaxPayloadSize: 1400}}
					ep.gso = stack.GSO{Type: stack.GSOTCPv4, MSS: 1400}
					err := ep.connect(tcpip.FullAddress{Addr: remote, Port: 8080}, false)
					if _, ok := err.(*tcpip.ErrConnectStarted); !ok {
						t.Fatalf("restored connect: %v", err)
					}
					if got, want := ep.snd.gso, supported != stack.GSONotSupported; got != want {
						t.Errorf("sender GSO=%t, want %t", got, want)
					}
					if supported == stack.GSONotSupported {
						if ep.gso.Type != stack.GSONone {
							t.Errorf("stale GSO state: %+v", ep.gso)
						}
					} else if ep.gso.MSS == 0 || int(ep.gso.MSS) != ep.snd.MaxPayloadSize {
						t.Errorf("MSS=%d, sender payload=%d", ep.gso.MSS, ep.snd.MaxPayloadSize)
					}
					// Tear down the synthetic sender without driving an artificial FIN.
					network.UnregisterTransportEndpoint(ep.effectiveNetProtos, ProtocolNumber, ep.ID, ep, ep.boundPortFlags, ep.boundBindToDevice)
					ep.isRegistered = false
					ep.route.Release()
					ep.route = nil
					ep.state.Store(uint32(StateClose))
				})
			}
		}
	}
}
