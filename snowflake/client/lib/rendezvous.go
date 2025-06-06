// WebRTC rendezvous requires the exchange of SessionDescriptions between
// peers in order to establish a PeerConnection.

package lib

import (
	"bufio"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	utls "github.com/refraction-networking/utls"
	"github.com/yl2chen/cidranger"

	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/certs"
	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/event"
	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/messages"
	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/nat"
	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/util"
	utlsutil "gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/utls"
)

const (
	brokerErrorUnexpected string = "Unexpected error, no answer."
	rendezvousErrorMsg    string = "One of SQS, AmpCache, or Domain Fronting rendezvous methods must be used."

	readLimit = 100000 //Maximum number of bytes to be read from an HTTP response
)

// RendezvousMethod represents a way of communicating with the broker: sending
// an encoded client poll request (SDP offer) and receiving an encoded client
// poll response (SDP answer) in return. RendezvousMethod is used by
// BrokerChannel, which is in charge of encoding and decoding, and all other
// tasks that are independent of the rendezvous method.
type RendezvousMethod interface {
	Exchange([]byte) ([]byte, error)
}

// BrokerChannel uses a RendezvousMethod to communicate with the Snowflake broker.
// The BrokerChannel is responsible for encoding and decoding SDP offers and answers;
// RendezvousMethod is responsible for the exchange of encoded information.
type BrokerChannel struct {
	Rendezvous         RendezvousMethod
	keepLocalAddresses bool
	natType            string
	lock               sync.Mutex
	BridgeFingerprint  string
}

// Paths to locally stored CAIDA datasets
const (
	caidaIPv4File = `/home/ubuntu/tor-snowflake/prefix-data/routeviews-rv2-20250310-1200.pfx2as`
	caidaIPv6File = `/home/ubuntu/tor-snowflake/prefix-data/routeviews-rv6-20250312-1200.pfx2as`
	logFilePath   = "proxy_ASNs.log"
)

// Global Data Structures
var (
	asnDataLock   sync.Mutex
	radixTreeV4   cidranger.Ranger // Radix Tree for IPv4
	radixTreeV6   cidranger.Ranger // Radix Tree for IPv6
	randomHMACKey []byte
)

// ASN Entry Struct
type asnEntry struct {
	cidr net.IPNet
	asn  string
}

// Implementing cidranger.RangerEntry for ASN Lookups
func (e asnEntry) Network() net.IPNet {
	return e.cidr
}

// Create IPv4 and IPv6 Radix Trees (CIDR Ranger)
func init() {
	radixTreeV4 = cidranger.NewPCTrieRanger()
	radixTreeV6 = cidranger.NewPCTrieRanger()

	errV4 := loadCAIDAData(caidaIPv4File, radixTreeV4)
	errV6 := loadCAIDAData(caidaIPv6File, radixTreeV6)

	if errV4 != nil {
		fmt.Println("Error loading IPv4 CAIDA data:", errV4)
	}
	if errV6 != nil {
		fmt.Println("Error loading IPv6 CAIDA data:", errV6)
	}

	randomHMACKey = generateRandomKey() // generate key upon initialization
	//continue
}

// Generates new random key upon each iteration
func generateRandomKey() []byte {
	key := make([]byte, 32) // 256-bit key
	_, err := rand.Read(key)
	if err != nil {
		log.Fatalf("Failed to generate HMAC key: %v", err)
	}
	fmt.Println("Initialized encryption key, loaded CAIDA datasets")
	return key
}

// Hash IP using keyed SHA-256 to anonymize stored IP addresses
func hashIP(ip string) string {
	mac := hmac.New(sha256.New, randomHMACKey)
	mac.Write([]byte(ip))
	return hex.EncodeToString(mac.Sum(nil))
}

// We make a copy of DefaultTransport because we want the default Dial
// and TLSHandshakeTimeout settings. But we want to disable the default
// ProxyFromEnvironment setting.
func createBrokerTransport(proxy *url.URL) http.RoundTripper {
	tlsConfig := &tls.Config{
		RootCAs: certs.GetRootCAs(),
	}
	transport := &http.Transport{TLSClientConfig: tlsConfig}
	transport.Proxy = nil
	if proxy != nil {
		transport.Proxy = http.ProxyURL(proxy)
	}
	transport.ResponseHeaderTimeout = 15 * time.Second
	return transport
}

func NewBrokerChannelFromConfig(config ClientConfig) (*BrokerChannel, error) {
	log.Println("Rendezvous using Broker at:", config.BrokerURL)

	if len(config.FrontDomains) != 0 {
		log.Printf("Domain fronting using a randomly selected domain from: %v", config.FrontDomains)
	}

	brokerTransport := createBrokerTransport(config.CommunicationProxy)

	if config.UTLSClientID != "" {
		utlsClientHelloID, err := utlsutil.NameToUTLSID(config.UTLSClientID)
		if err != nil {
			return nil, fmt.Errorf("unable to create broker channel: %w", err)
		}
		utlsConfig := &utls.Config{
			RootCAs: certs.GetRootCAs(),
		}
		brokerTransport = utlsutil.NewUTLSHTTPRoundTripperWithProxy(utlsClientHelloID, utlsConfig, brokerTransport,
			config.UTLSRemoveSNI, config.CommunicationProxy)
	}

	var rendezvous RendezvousMethod
	var err error
	if config.SQSQueueURL != "" {
		if config.AmpCacheURL != "" || config.BrokerURL != "" {
			log.Fatalln("Multiple rendezvous methods specified. " + rendezvousErrorMsg)
		}
		if config.SQSCredsStr == "" {
			log.Fatalln("sqscreds must be specified to use SQS rendezvous method.")
		}
		log.Println("Through SQS queue at:", config.SQSQueueURL)
		rendezvous, err = newSQSRendezvous(config.SQSQueueURL, config.SQSCredsStr, brokerTransport)
	} else if config.AmpCacheURL != "" && config.BrokerURL != "" {
		log.Println("Through AMP cache at:", config.AmpCacheURL)
		rendezvous, err = newAMPCacheRendezvous(
			config.BrokerURL, config.AmpCacheURL, config.FrontDomains,
			brokerTransport)
	} else if config.BrokerURL != "" {
		rendezvous, err = newHTTPRendezvous(
			config.BrokerURL, config.FrontDomains, brokerTransport)
	} else {
		log.Fatalln("No rendezvous method was specified. " + rendezvousErrorMsg)
	}
	if err != nil {
		return nil, err
	}

	return &BrokerChannel{
		Rendezvous:         rendezvous,
		keepLocalAddresses: config.KeepLocalAddresses,
		natType:            nat.NATUnknown,
		BridgeFingerprint:  config.BridgeFingerprint,
	}, nil
}

// Negotiate uses a RendezvousMethod to send the client's WebRTC SDP offer
// and receive a snowflake proxy WebRTC SDP answer in return.
func (bc *BrokerChannel) Negotiate(offer *webrtc.SessionDescription) (*webrtc.SessionDescription, error) {
	fmt.Println("Negotiate with broker") // Debugging Log

	if !bc.keepLocalAddresses {
		offer = &webrtc.SessionDescription{
			Type: offer.Type,
			SDP:  util.StripLocalAddresses(offer.SDP),
		}
	}

	offerSDP, err := util.SerializeSessionDescription(offer)
	if err != nil {
		return nil, err
	}

	// Encode the client poll request.
	bc.lock.Lock()
	req := &messages.ClientPollRequest{
		Offer:       offerSDP,
		NAT:         bc.natType,
		Fingerprint: bc.BridgeFingerprint,
	}
	encReq, err := req.EncodeClientPollRequest()
	bc.lock.Unlock()
	if err != nil {
		return nil, err
	}

	fmt.Println("Sending request to broker") // Debugging Log

	// Do the exchange using our RendezvousMethod.
	encResp, err := bc.Rendezvous.Exchange(encReq)
	if err != nil {
		return nil, err
	}
	//log.Printf("Received answer: %s", string(encResp))

	fmt.Println("Received response from broker") // Debugging Log

	// Decode the client poll response.
	resp, err := messages.DecodeClientPollResponse(encResp)
	if err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return nil, errors.New(resp.Error)
	}

	// Original Implementation: return deserialized data
	// Modification: use deserialized data to find IP address of proxy

	// Deserialize the WebRTC session description
	answer, err := util.DeserializeSessionDescription(resp.Answer)
	if err == nil {
		ip := extractIP(answer.SDP)

		// Set answer to null
		answer = nil

		if ip == "" {
			fmt.Println("No IP address found in answer.")
		} else {
			asn := getASN(ip)
			fmt.Println("ASN recorded")

			logASN(ip, asn) // Performs IP hashing
			ip = ""

			// Sleep for 5 seconds
			time.Sleep(5 * time.Second)
		}
	}

	// Return an Error to indicate end of program and restart execution
	return nil, fmt.Errorf("- intentional error, retrying now")
}

// Extract IP Address from SDP
func extractIP(sdp string) string {
	reConn := regexp.MustCompile(`c=IN IP(?:4|6) ([0-9a-fA-F:.]+)`)
	matches := reConn.FindStringSubmatch(sdp)
	if len(matches) > 1 && matches[1] != "0.0.0.0" {
		defer func() { matches[1] = "" }() // Zero memory before exit - additional resistance against debugging
		return matches[1]
	}

	reCand := regexp.MustCompile(`a=candidate:[^ ]+ [0-9]+ udp [0-9]+ ([0-9a-fA-F:.]+)`)
	matches = reCand.FindStringSubmatch(sdp)
	if len(matches) > 1 {
		defer func() { matches[1] = "" }() // Zero memory before exit - additional resistance against debugging
		return matches[1]
	}

	return ""
}

// Lookup ASN using Radix Tree (Longest Prefix Match)
func getASN(ip string) string {
	parsedIP := net.ParseIP(ip)
	ip = ""
	if parsedIP == nil {
		return "ASN not found"
	}

	asnDataLock.Lock()
	defer asnDataLock.Unlock()

	var tree cidranger.Ranger
	if parsedIP.To4() != nil {
		tree = radixTreeV4 // IPv4 lookup
	} else {
		tree = radixTreeV6 // IPv6 lookup
	}

	entries, err := tree.ContainingNetworks(parsedIP)
	if err != nil || len(entries) == 0 {
		return "ASN not found"
	}
	parsedIP = nil

	// Select the longest matching prefix
	longestEntry := entries[0].(asnEntry)
	return longestEntry.asn
}

// Log hashed IP and ASN pairs to a CSV file in the format: timestamp, hashedIP, ASN
func logASN(ip, asn string) {
	hashedIP := hashIP(ip)
	ip = ""

	// Open CSV file for appending
	f, err := os.OpenFile("proxy_ASNs.csv", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Println("Error opening CSV file:", err)
		return
	}
	defer f.Close()

	// Create a CSV-formatted line: timestamp (formatted), hashedIP, ASN
	timestamp := time.Now().Format(time.RFC3339)
	line := fmt.Sprintf("%s,%s,%s\n", timestamp, hashedIP, asn)

	if _, err := f.WriteString(line); err != nil {
		fmt.Println("Error writing to CSV file:", err)
	}
}

// Load CAIDA IP Prefix to ASN Data into the Radix Tree
func loadCAIDAData(filePath string, tree cidranger.Ranger) error {
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("error opening CAIDA dataset: %v", err)
	}
	defer file.Close()

	reader := bufio.NewScanner(file)
	count := 0

	for reader.Scan() {
		line := strings.Fields(reader.Text())
		if len(line) < 3 {
			continue // Ensure we have at least 3 columns: Prefix, Mask, ASN
		}

		prefix := strings.TrimSpace(line[0])
		mask := strings.TrimSpace(line[1])
		asn := strings.TrimSpace(line[2])

		// Convert to CIDR format
		cidr := fmt.Sprintf("%s/%s", prefix, mask)

		// Validate and insert into the radix tree
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}

		// Store entry in the appropriate tree
		entry := asnEntry{cidr: *network, asn: asn}
		tree.Insert(entry)
		count++
	}

	if err := reader.Err(); err != nil {
		return fmt.Errorf("error reading CAIDA dataset: %v", err)
	}

	fmt.Printf("CAIDA IP-to-ASN data loaded. Total prefixes: %d\n", count)
	return nil
}

// SetNATType sets the NAT type of the client so we can send it to the WebRTC broker.
// Sets NAT Type to either "unrestricted" or "restricted"
func (bc *BrokerChannel) SetNATType(NATType string) {
	fmt.Println("Setting NAT Type") // Debugging Log
	//NATType = "restricted"
	//NATType = "unrestricted"
	hardcodedNATType := "restricted"

	bc.lock.Lock()
	bc.natType = NATType
	bc.natType = hardcodedNATType
	bc.lock.Unlock()
	log.Printf("Original NAT Type: %s", NATType)
	fmt.Printf("Hard Coded NAT Type: %s \n", hardcodedNATType)
}

// WebRTCDialer implements the |Tongue| interface to catch snowflakes, using BrokerChannel.
type WebRTCDialer struct {
	*BrokerChannel
	webrtcConfig *webrtc.Configuration
	max          int

	eventLogger event.SnowflakeEventReceiver
	proxy       *url.URL
}

// Deprecated: Use NewWebRTCDialerWithEventsAndProxy instead
func NewWebRTCDialer(broker *BrokerChannel, iceServers []webrtc.ICEServer, max int) *WebRTCDialer {
	return NewWebRTCDialerWithEventsAndProxy(broker, iceServers, max, nil, nil)
}

// Deprecated: Use NewWebRTCDialerWithEventsAndProxy instead
func NewWebRTCDialerWithEvents(broker *BrokerChannel, iceServers []webrtc.ICEServer, max int, eventLogger event.SnowflakeEventReceiver) *WebRTCDialer {
	return NewWebRTCDialerWithEventsAndProxy(broker, iceServers, max, eventLogger, nil)
}

// NewWebRTCDialerWithEventsAndProxy constructs a new WebRTCDialer.
func NewWebRTCDialerWithEventsAndProxy(broker *BrokerChannel, iceServers []webrtc.ICEServer, max int,
	eventLogger event.SnowflakeEventReceiver, proxy *url.URL,
) *WebRTCDialer {
	config := webrtc.Configuration{
		ICEServers: iceServers,
	}

	return &WebRTCDialer{
		BrokerChannel: broker,
		webrtcConfig:  &config,
		max:           max,

		eventLogger: eventLogger,
		proxy:       proxy,
	}
}

// Catch initializes a WebRTC Connection by signaling through the BrokerChannel.
func (w WebRTCDialer) Catch() (*WebRTCPeer, error) {
	// TODO: [#25591] Fetch ICE server information from Broker.
	// TODO: [#25596] Consider TURN servers here too.
	return NewWebRTCPeerWithEventsAndProxy(w.webrtcConfig, w.BrokerChannel, w.eventLogger, w.proxy)
}

// GetMax returns the maximum number of snowflakes to collect.
func (w WebRTCDialer) GetMax() int {
	return w.max
}
