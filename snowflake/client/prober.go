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

	// Create config object with broker url and front domains
	config := lib.ClientConfig{
		BrokerURL:          "https://1098762253.rsc.cdn77.org", // Broker URL to send WebRTC offers to
		FrontDomains:       []string{"www.phpmyadmin.net"},     // fronting domain to reach broker
		KeepLocalAddresses: false,
	}

	// Create a reusable BrokerChannel for communicating with the broker
	broker, err := lib.NewBrokerChannelFromConfig(config)
	if err != nil {
		log.Fatalf("Failed to create BrokerChannel: %v", err)
	}

	// Define reusable WebRTC ICE server configuration
	webrtcConfig := &webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				// Public STUN server for NAT traversal
				URLs: []string{"stun:stun.antisip.com:3478"},
			},
		},
	}

	// infinite loop for probing new proxies
	for {
		// Create a new WebRTC peer connection per iteration
		peerConnection, err := webrtc.NewPeerConnection(*webrtcConfig)
		if err != nil {
			log.Fatalf("Failed to create PeerConnection: %v", err)
		}

		// Create a data channel
		_, err = peerConnection.CreateDataChannel("data", nil)
		if err != nil {
			peerConnection.Close() // clean up on failure
			log.Fatalf("Failed to create DataChannel: %v", err)
		}

		// Generate WebRTC SDP offer to initiate connection
		offer, err := peerConnection.CreateOffer(nil)
		if err != nil {
			peerConnection.Close() // clean up on failure
			log.Fatalf("Failed to create offer: %v", err)
		}

		// Set local description
		err = peerConnection.SetLocalDescription(offer)
		if err != nil {
			peerConnection.Close() // clean up on failure
			log.Fatalf("Failed to set local description: %v", err)
		}

		// Call negotiate (returns only error)
		_, err = broker.Negotiate(&offer)
		if err != nil {
			log.Printf("Negotiate(): %v", err)
		}

		// Close the peer connection
		peerConnection.Close()

		// Sleep before the next iteration
		time.Sleep(10 * time.Second)
	}
}
