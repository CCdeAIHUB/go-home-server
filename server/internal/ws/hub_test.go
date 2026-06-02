package ws

import (
	"net"
	"reflect"
	"testing"
	"time"

	"gohome/shared/security"
	"gohome/shared/tunnel"
)

func TestValidateTimeKeyTracksFailures(t *testing.T) {
	session := &Session{failures: 2}
	now := time.Now()
	if reply := validateTimeKey(session, "ping-1", "secret", security.GenerateTimeKey("secret", now), now.Unix()); reply != nil {
		t.Fatalf("valid time key rejected: %+v", reply.Error)
	}
	if session.failures != 0 {
		t.Fatalf("valid time key did not reset failures: %d", session.failures)
	}
	if reply := validateTimeKey(session, "ping-2", "secret", "bad", now.Unix()); reply == nil || reply.Error == nil || reply.Error.Code != "time_key_invalid" {
		t.Fatalf("invalid time key reply got %+v", reply)
	}
	if session.failures != 1 {
		t.Fatalf("invalid time key failure count got %d want 1", session.failures)
	}
}

func TestPeerCandidatesPreferObservedAndFilterIPv6(t *testing.T) {
	session := &Session{
		observedEndpoint: "203.0.113.10:51000",
		remote:           "198.51.100.7:44322",
		udpPort:          47777,
	}
	got := peerCandidates(session)
	want := []string{
		"203.0.113.10:51000",
		"203.0.113.10:47777",
		"198.51.100.7:47777",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("peerCandidates got %#v want %#v", got, want)
	}

	ipv6Only := &Session{
		observedEndpoint: "[2001:db8::1]:51000",
		remote:           "[2001:db8::2]:44322",
		udpPort:          47777,
	}
	if got := peerCandidates(ipv6Only); len(got) != 0 {
		t.Fatalf("IPv6 endpoints must be filtered, got %#v", got)
	}
}

func TestHandleUDPPacketRecordsObservedSourceEndpoint(t *testing.T) {
	session := &Session{deviceID: "device-1", token: "token-1", udpPort: 47777}
	hub := &Hub{devices: map[string]*Session{"device-1": session}}
	packet, err := tunnel.MarshalRegister(tunnel.Register{DeviceID: "device-1", Token: "token-1"})
	if err != nil {
		t.Fatal(err)
	}

	ack := hub.handleUDPPacket(packet, &net.UDPAddr{IP: net.ParseIP("203.0.113.20"), Port: 62000})

	if session.observedEndpoint != "203.0.113.20:62000" {
		t.Fatalf("observed endpoint got %q", session.observedEndpoint)
	}
	if ack == nil || ack.ObservedEndpoint != "203.0.113.20:62000" {
		t.Fatalf("register ack got %+v", ack)
	}
	got := peerCandidates(session)
	want := []string{"203.0.113.20:62000", "203.0.113.20:47777"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("peerCandidates got %#v want %#v", got, want)
	}
}
