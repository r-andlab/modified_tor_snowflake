package main

import (
	"fmt"
	"log"
	"time"

	"github.com/pion/webrtc/v4"
	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/client/lib"
)

func main() {
	fmt.Println("--- Starting Snowflake Prober ---")

	// Here, modified rendezvous.go init() will:
	// - Load CAIDA data
	// - Generate HMAC key
	// - Set up radix trees

	for {
		// Create config object with broker url and front domains
		config := lib.ClientConfig{
			BrokerURL:    "https://1098762253.rsc.cdn77.org", // from torrc
			FrontDomains: []string{"www.phpmyadmin.net"},     // from torrc
			//UTLSClientID:  "hellorandomizedalpn",              // optional
			KeepLocalAddresses: false,
		}

		// Build broker channel to send webrtc offer
		broker, err := lib.NewBrokerChannelFromConfig(config)
		if err != nil {
			log.Printf("Failed to create BrokerChannel: %v", err)
			time.Sleep(10 * time.Second) // sleep upon error to avoid possible congestion
			continue
		}

		// Specify webrtc config object for associating ICE servers
		webrtcConfig := &webrtc.Configuration{
			ICEServers: []webrtc.ICEServer{
				{
					URLs: []string{"stun:stun.antisip.com:3478"},
				},
			},
		}

		// Create new webrtc peer connection, using prev ICE servers
		// Like a socket to facilitate the end-to-end communication
		peerConnection, err := webrtc.NewPeerConnection(*webrtcConfig)
		if err != nil {
			log.Fatalf("Failed to create PeerConnection: %v", err)
		}

		// Create webrtc data channel between prober and peer
		_, err = peerConnection.CreateDataChannel("data", nil)
		if err != nil {
			log.Fatalf("Failed to create DataChannel: %v", err)
		}

		// Generate webrtc offer: Session Description Protocol
		offer, err := peerConnection.CreateOffer(nil)
		if err != nil {
			log.Fatalf("Failed to create offer: %v", err)
		}

		// Set offer (required by webrtc)
		err = peerConnection.SetLocalDescription(offer)
		if err != nil {
			log.Fatalf("Failed to set local description: %v", err)
		}

		// Send offer to broker: Negotiate()
		// Try to connect, log IP/ASN, fail and return error, retry
		_, err = broker.Negotiate(&offer)
		if err != nil {
			log.Printf("Negotiate(): %v", err)
		}
	}
}
