package protocols

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/antongulenko/RTP/helpers"
)

const (
	ErrorChanBuffer = 16
)

type Server struct {
	stopped    *helpers.OneshotCondition
	listenConn *net.UDPConn
	errors     chan error
	handler    ServerHandler

	// For sending heartbeats
	heartbeatRunning  *helpers.OneshotCondition
	heartbeatReceiver *net.UDPAddr
	heartbeatTimeout  time.Duration
	heartbeatSeq      uint64

	// For receiving heartbeats
	HeartbeatHandler func(source *net.UDPAddr, beat *HeartbeatPacket)

	Wg        *sync.WaitGroup
	Stopped   bool
	LocalAddr *net.UDPAddr
}

type ServerHandler interface {
	Protocol
	HandleRequest(request *Packet)
	StopServer()
}

func NewServer(local_addr string, handler ServerHandler) (*Server, error) {
	if handler == nil {
		return nil, fmt.Errorf("Need non-nil ServerHandler")
	}
	udpAddr, err := net.ResolveUDPAddr("udp4", local_addr)
	if err != nil {
		return nil, err
	}
	listenConn, err := net.ListenUDP("udp4", udpAddr)
	if err != nil {
		return nil, err
	}
	localUdpAddr, ok := listenConn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return nil, fmt.Errorf("Failed to convert to *net.UdpAddr: %v", listenConn.LocalAddr())
	}
	return &Server{
		Wg:               new(sync.WaitGroup),
		LocalAddr:        localUdpAddr,
		handler:          handler,
		listenConn:       listenConn,
		errors:           make(chan error, ErrorChanBuffer),
		stopped:          helpers.NewOneshotCondition(),
		heartbeatRunning: helpers.NewOneshotCondition(),
	}, nil
}

func (server *Server) String() string {
	return fmt.Sprintf("%v on %v", server.handler.Name(), server.LocalAddr)
}

func (server *Server) Errors() <-chan error {
	return server.errors
}

func (server *Server) Start() {
	server.Wg.Add(1)
	go server.listen()
}

func (server *Server) Observe(wg *sync.WaitGroup) <-chan interface{} {
	return server.stopped.Observe(wg)
}

func (server *Server) Stop() {
	server.stopped.Enable(func() {
		server.Stopped = true
		server.listenConn.Close()
		server.handler.StopServer()
		server.Wg.Wait()
	})
}

func (server *Server) listen() {
	defer server.Wg.Done()
	for {
		if server.Stopped {
			return
		}
		packet, err := receivePacket(server.listenConn, 0, server.handler)
		if err != nil {
			if server.Stopped {
				return // error because of read from closed connection
			}
			server.LogError(err)
			server.ReplyError(packet, err)
		} else {
			server.handle(packet)
		}
	}
}

func (server *Server) handle(request *Packet) {
	val := request.Val
	switch request.Code {
	case CodeOK:
		server.LogError(errors.New("Received standalone OK message"))
	case CodeError:
		server.LogError(fmt.Errorf("Received standalone Error: %s", request.Error()))
	case CodePong:
		server.LogError(fmt.Errorf("Received standalone Pong message"))
	case CodePing:
		if ping, ok := val.(*PingPacket); ok {
			server.Reply(request, CodePong, ping.PongValue())
		} else {
			err := fmt.Errorf("%s Ping received with wrong payload: (%T) %v", server.handler.Name(), val, val)
			server.ReplyError(request, err)
		}
	case CodeConfigureHeartbeat:
		if conf, ok := val.(*ConfigureHeartbeatPacket); ok {
			server.ReplyCheck(request, server.ConfigureHeartbeat(conf.TargetServer, conf.Timeout))
		} else {
			err := fmt.Errorf("%s ConfigureHeartbeat received with wrong payload: (%T) %v", server.handler.Name(), val, val)
			server.ReplyError(request, err)
		}
	case CodeHeartbeat:
		if beat, ok := val.(*HeartbeatPacket); ok {
			if server.HeartbeatHandler != nil {
				server.HeartbeatHandler(request.SourceAddr, beat)
			} else {
				server.LogError(fmt.Errorf("Received Heartbeat from %v, but HeartbeatHandler is not configured", request.SourceAddr))
			}
		} else {
			server.LogError(fmt.Errorf("%s Heartbeat received with wrong payload: (%T) %v", server.handler.Name(), val, val))
		}
	default:
		server.handler.HandleRequest(request)
	}
}

func (server *Server) Reply(request *Packet, code uint, value interface{}) {
	packet := Packet{Code: code, Val: value}
	err := packet.sendPacket(server.listenConn, request.SourceAddr, server.handler)
	if err != nil {
		server.LogError(fmt.Errorf("Failed to send reply: %v", err))
	}
}

func (server *Server) ReplyCheck(request *Packet, err error) {
	if err == nil {
		server.ReplyOK(request)
	} else {
		server.ReplyError(request, err)
	}
}

func (server *Server) ReplyOK(request *Packet) {
	server.Reply(request, CodeOK, "")
}

func (server *Server) ReplyError(request *Packet, err error) {
	server.Reply(request, CodeError, err.Error())
}

func (server *Server) LogError(err error) {
	select {
	case server.errors <- err:
	default:
		log.Printf("Warning: dropped server error: %v\n", err)
	}
}

func (server *Server) ConfigureHeartbeat(receiver string, timeout time.Duration) error {
	var addr *net.UDPAddr
	if receiver != "" {
		var err error
		addr, err = net.ResolveUDPAddr("udp4", receiver)
		if err != nil {
			return fmt.Errorf("Failed to resolve heartbeat-receiver %s: %v", receiver, err)
		}
	}

	// TODO potential race condition. This should only be called once.
	// TODO once started the sendHeartbeats routine will keep spinning even if heartbeats are disabled again
	server.heartbeatTimeout = timeout
	server.heartbeatReceiver = addr
	server.heartbeatSeq = 0
	if addr != nil && timeout > 0 {
		// Not really an error.
		server.LogError(fmt.Errorf("Sending heartbeats to %s every %v", server.heartbeatReceiver, timeout))
		server.heartbeatRunning.Enable(server.sendHeartbeats)
	} else {
		server.LogError(fmt.Errorf("Stopped sending heartbeats"))
	}
	return nil
}

func (server *Server) sendHeartbeats() {
	server.Wg.Add(1)
	go func() {
		defer server.Wg.Done()
		for !server.Stopped {
			receiver := server.heartbeatReceiver
			timeout := server.heartbeatTimeout
			if receiver != nil && timeout != 0 {
				packet := &Packet{
					Code: CodeHeartbeat,
					Val: HeartbeatPacket{
						TimeSent: time.Now(),
						Seq:      server.heartbeatSeq,
					},
				}
				// Special routine for sending heartbeats to allow using the server port as source address
				server.heartbeatSeq++
				err := packet.sendPacket(server.listenConn, receiver, EmptyProtocol)
				if server.Stopped {
					break
				}
				if err != nil {
					server.LogError(fmt.Errorf("Error sending heartbeat to %v: %v", receiver, err))
				}
			}
			if timeout == 0 {
				timeout = 1 * time.Second
			}
			time.Sleep(timeout)
		}
	}()
}

func ParseServerFlags(default_ip string, default_port int) string {
	port := flag.Int("port", default_port, "The port to start the server")
	ip := flag.String("host", default_ip, "The ip to listen for traffic")
	//heartbeatReceiver := flag.String("heartbeat", "", "The server to send heartbeats to")
	flag.Parse()
	return net.JoinHostPort(*ip, strconv.Itoa(int(*port)))
}
