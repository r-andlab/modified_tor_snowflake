package lib

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net"
	"net/url"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/transport/v3"
	"github.com/pion/transport/v3/stdnet"
	"github.com/pion/webrtc/v4"

	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/event"
	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/proxy"
	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/util"
)

// WebRTCPeer represents a WebRTC connection to a remote snowflake proxy.
type WebRTCPeer struct {
	id        string
	pc        *webrtc.PeerConnection
	transport *webrtc.DataChannel

	recvPipe  *io.PipeReader
	writePipe *io.PipeWriter

	mu          sync.Mutex
	lastReceive time.Time

	open   chan struct{}
	closed chan struct{}

	once sync.Once

	bytesLogger  bytesLogger
	eventsLogger event.SnowflakeEventReceiver
	proxy        *url.URL
}

func NewWebRTCPeerWithEventsAndProxy(
	config *webrtc.Configuration, broker *BrokerChannel,
	eventsLogger event.SnowflakeEventReceiver, proxy *url.URL,
) (*WebRTCPeer, error) {
	if eventsLogger == nil {
		eventsLogger = event.NewSnowflakeEventDispatcher()
	}

	connection := new(WebRTCPeer)
	{
		var buf [8]byte
		if _, err := rand.Read(buf[:]); err != nil {
			panic(err)
		}
		connection.id = "snowflake-" + hex.EncodeToString(buf[:])
	}
	connection.closed = make(chan struct{})
	connection.bytesLogger = &bytesNullLogger{}
	connection.recvPipe, connection.writePipe = io.Pipe()
	connection.eventsLogger = eventsLogger
	connection.proxy = proxy

	err := connection.connect(config, broker)
	if err != nil {
		connection.Close()
		return nil, err
	}
	return connection, nil
}

func (c *WebRTCPeer) Read(b []byte) (int, error) {
	return c.recvPipe.Read(b)
}

func (c *WebRTCPeer) Write(b []byte) (int, error) {
	err := c.transport.Send(b)
	if err != nil {
		return 0, err
	}
	c.bytesLogger.addOutbound(int64(len(b)))
	return len(b), nil
}

func (c *WebRTCPeer) Closed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

func (c *WebRTCPeer) Close() error {
	c.once.Do(func() {
		close(c.closed)
		c.cleanup()
		log.Printf("WebRTC: Closing")
	})
	return nil
}

func (c *WebRTCPeer) checkForStaleness(timeout time.Duration) {
	c.mu.Lock()
	c.lastReceive = time.Now()
	c.mu.Unlock()
	for {
		c.mu.Lock()
		lastReceive := c.lastReceive
		c.mu.Unlock()
		if time.Since(lastReceive) > timeout {
			log.Printf("WebRTC: No messages received for %v -- closing stale connection.", timeout)
			err := errors.New("no messages received, closing stale connection")
			c.eventsLogger.OnNewSnowflakeEvent(event.EventOnSnowflakeConnectionFailed{Error: err})
			c.Close()
			return
		}
		select {
		case <-c.closed:
			return
		case <-time.After(time.Second):
		}
	}
}

func (c *WebRTCPeer) connect(
	config *webrtc.Configuration,
	broker *BrokerChannel,
) error {
	log.Println(c.id, " connecting...")

	err := c.preparePeerConnection(config, broker.keepLocalAddresses)
	localDescription := c.pc.LocalDescription()
	c.eventsLogger.OnNewSnowflakeEvent(event.EventOnOfferCreated{
		WebRTCLocalDescription: localDescription,
		Error:                  err,
	})
	if err != nil {
		return err
	}

	answer, err := broker.Negotiate(localDescription)
	c.eventsLogger.OnNewSnowflakeEvent(event.EventOnBrokerRendezvous{
		WebRTCRemoteDescription: answer,
		Error:                   err,
	})
	if err != nil {
		return err
	}
	log.Printf("Received Answer.\n")
	err = c.pc.SetRemoteDescription(*answer)
	if err != nil {
		log.Println("WebRTC: Unable to SetRemoteDescription:", err)
		return err
	}

	select {
	case <-c.open:
	case <-time.After(DataChannelTimeout):
		c.transport.Close()
		err := errors.New("timeout waiting for DataChannel.OnOpen")
		c.eventsLogger.OnNewSnowflakeEvent(event.EventOnSnowflakeConnectionFailed{Error: err})
		return err
	}

	go c.checkForStaleness(SnowflakeTimeout)
	return nil
}

func (c *WebRTCPeer) preparePeerConnection(
	config *webrtc.Configuration,
	keepLocalAddresses bool,
) error {
	s := webrtc.SettingEngine{}
	if !keepLocalAddresses {
		s.SetIPFilter(func(ip net.IP) bool {
			return !util.IsLocal(ip) && !ip.IsLoopback() && !ip.IsUnspecified()
		})
		s.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	}
	s.SetIncludeLoopbackCandidate(keepLocalAddresses)
	var vnet transport.Net
	vnet, _ = stdnet.NewNet()
	if c.proxy != nil {
		if err := proxy.CheckProxyProtocolSupport(c.proxy); err != nil {
			return err
		}
		socksClient := proxy.NewSocks5UDPClient(c.proxy)
		vnet = proxy.NewTransportWrapper(&socksClient, vnet)
	}
	s.SetNet(vnet)
	api := webrtc.NewAPI(webrtc.WithSettingEngine(s))
	var err error
	c.pc, err = api.NewPeerConnection(*config)
	if err != nil {
		log.Printf("NewPeerConnection ERROR: %s", err)
		return err
	}
	ordered := true
	dataChannelOptions := &webrtc.DataChannelInit{
		Ordered: &ordered,
	}
	dc, err := c.pc.CreateDataChannel(c.id, dataChannelOptions)
	if err != nil {
		log.Printf("CreateDataChannel ERROR: %s", err)
		return err
	}
	dc.OnOpen(func() {
		c.eventsLogger.OnNewSnowflakeEvent(event.EventOnSnowflakeConnected{})
		log.Println("WebRTC: DataChannel.OnOpen")
		close(c.open)
	})
	dc.OnClose(func() {
		log.Println("WebRTC: DataChannel.OnClose")
		c.Close()
	})
	dc.OnError(func(err error) {
		c.eventsLogger.OnNewSnowflakeEvent(event.EventOnSnowflakeConnectionFailed{Error: err})
	})
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		n, err := c.writePipe.Write(msg.Data)
		c.bytesLogger.addInbound(int64(n))
		if err != nil {
			log.Println("Error writing to SOCKS pipe")
			if inerr := c.writePipe.CloseWithError(err); inerr != nil {
				log.Printf("c.writePipe.CloseWithError returned error: %v", inerr)
			}
		}
		c.mu.Lock()
		c.lastReceive = time.Now()
		c.mu.Unlock()
	})
	c.transport = dc
	c.open = make(chan struct{})
	log.Println("WebRTC: DataChannel created")

	offer, err := c.pc.CreateOffer(nil)
	if err != nil {
		log.Println("Failed to prepare offer", err)
		c.pc.Close()
		return err
	}
	log.Println("WebRTC: Created offer")
	done := webrtc.GatheringCompletePromise(c.pc)
	err = c.pc.SetLocalDescription(offer)
	if err != nil {
		log.Println("Failed to apply offer", err)
		c.pc.Close()
		return err
	}
	log.Println("WebRTC: Set local description")
	<-done
	return nil
}

func (c *WebRTCPeer) cleanup() {
	if c.writePipe != nil {
		c.writePipe.Close()
	}
	if c.transport != nil {
		log.Printf("WebRTC: closing DataChannel")
		c.transport.Close()
	}
	if c.pc != nil {
		log.Printf("WebRTC: closing PeerConnection")
		err := c.pc.Close()
		if err != nil {
			log.Printf("Error closing peerconnection...")
		}
	}
}
