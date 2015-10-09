package heartbeat

import (
	"encoding/gob"
	"fmt"
	"time"

	"github.com/antongulenko/RTP/protocols"
)

var (
	Protocol     *heartbeatProtocol
	MiniProtocol = protocols.NewMiniProtocol(Protocol)
)

// ======================= Packets =======================

const (
	codeHeartbeat = protocols.Code(iota + 6)
	codeConfigureHeartbeat
)

type HeartbeatPacket struct {
	TimeSent time.Time
	Seq      uint64
}

type ConfigureHeartbeatPacket struct {
	TargetServer string
	Timeout      time.Duration
}

// ======================= Protocol =======================

type heartbeatProtocol struct {
}

func (*heartbeatProtocol) Name() string {
	return "Heartbeat"
}

func (proto *heartbeatProtocol) Decoders() protocols.DecoderMap {
	return protocols.DecoderMap{
		codeHeartbeat:          proto.decodeHeartbeat,
		codeConfigureHeartbeat: proto.decodeConfigureHeartbeat,
	}
}

func (proto *heartbeatProtocol) decodeHeartbeat(decoder *gob.Decoder) (interface{}, error) {
	var val HeartbeatPacket
	err := decoder.Decode(&val)
	if err != nil {
		return nil, fmt.Errorf("Error decoding Heartbeat value: %v", err)
	}
	return &val, nil
}

func (proto *heartbeatProtocol) decodeConfigureHeartbeat(decoder *gob.Decoder) (interface{}, error) {
	var val ConfigureHeartbeatPacket
	err := decoder.Decode(&val)
	if err != nil {
		return nil, fmt.Errorf("Error decoding ConfigureHeartbeat value: %v", err)
	}
	return &val, nil
}

// ======================= Client =======================

type Client struct {
	protocols.Client
}

func NewClient(client protocols.Client) (*Client, error) {
	if err := client.Protocol().CheckIncludesFragment(Protocol.Name()); err != nil {
		return nil, err
	}
	return &Client{client}, nil
}

func NewClientFor(server_addr string) (*Client, error) {
	client, err := protocols.NewMiniClientFor(server_addr, Protocol)
	if err != nil {
		return nil, err
	}
	return &Client{client}, nil
}

func (client *Client) ConfigureHeartbeat(receiver *protocols.Server, timeout time.Duration) error {
	packet := ConfigureHeartbeatPacket{
		TargetServer: receiver.LocalAddr.String(),
		Timeout:      timeout,
	}
	// Do not wait for a reply.
	return client.Send(codeConfigureHeartbeat, packet)
}
