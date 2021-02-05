package core

import (
	"context"
	"crypto/tls"
	"errors"
	"github.com/lucas-clemente/quic-go"
	"github.com/lunixbochs/struc"
	"github.com/tobyxdd/hysteria/pkg/acl"
	"github.com/tobyxdd/hysteria/pkg/utils"
	"net"
	"time"
)

const dialTimeout = 10 * time.Second

type AuthFunc func(addr net.Addr, auth []byte, sSend uint64, sRecv uint64) (bool, string)
type TCPRequestFunc func(addr net.Addr, auth []byte, reqAddr string, action acl.Action, arg string)
type TCPErrorFunc func(addr net.Addr, auth []byte, reqAddr string, err error)

type Server struct {
	sendBPS, recvBPS  uint64
	congestionFactory CongestionFactory
	aclEngine         *acl.Engine

	authFunc       AuthFunc
	tcpRequestFunc TCPRequestFunc
	tcpErrorFunc   TCPErrorFunc

	listener quic.Listener
}

func NewServer(addr string, tlsConfig *tls.Config, quicConfig *quic.Config,
	sendBPS uint64, recvBPS uint64, congestionFactory CongestionFactory, aclEngine *acl.Engine,
	obfuscator Obfuscator, authFunc AuthFunc, tcpRequestFunc TCPRequestFunc, tcpErrorFunc TCPErrorFunc) (*Server, error) {
	packetConn, err := net.ListenPacket("udp", addr)
	if err != nil {
		return nil, err
	}
	if obfuscator != nil {
		// Wrap PacketConn with obfuscator
		packetConn = &obfsPacketConn{
			Orig:       packetConn,
			Obfuscator: obfuscator,
		}
	}
	listener, err := quic.Listen(packetConn, tlsConfig, quicConfig)
	if err != nil {
		return nil, err
	}
	s := &Server{
		listener:          listener,
		sendBPS:           sendBPS,
		recvBPS:           recvBPS,
		congestionFactory: congestionFactory,
		aclEngine:         aclEngine,
		authFunc:          authFunc,
		tcpRequestFunc:    tcpRequestFunc,
		tcpErrorFunc:      tcpErrorFunc,
	}
	return s, nil
}

func (s *Server) Serve() error {
	for {
		cs, err := s.listener.Accept(context.Background())
		if err != nil {
			return err
		}
		go s.handleClient(cs)
	}
}

func (s *Server) Close() error {
	return s.listener.Close()
}

func (s *Server) handleClient(cs quic.Session) {
	// Expect the client to create a control stream to send its own information
	ctx, ctxCancel := context.WithTimeout(context.Background(), protocolTimeout)
	stream, err := cs.AcceptStream(ctx)
	ctxCancel()
	if err != nil {
		_ = cs.CloseWithError(closeErrorCodeProtocol, "protocol error")
		return
	}
	// Handle the control stream
	auth, ok, err := s.handleControlStream(cs, stream)
	if err != nil {
		_ = cs.CloseWithError(closeErrorCodeProtocol, "protocol error")
		return
	}
	if !ok {
		_ = cs.CloseWithError(closeErrorCodeAuth, "auth error")
		return
	}
	// Start accepting streams
	for {
		stream, err := cs.AcceptStream(context.Background())
		if err != nil {
			break
		}
		go s.handleStream(cs.RemoteAddr(), auth, stream)
	}
	_ = cs.CloseWithError(closeErrorCodeGeneric, "")
}

// Auth & negotiate speed
func (s *Server) handleControlStream(cs quic.Session, stream quic.Stream) ([]byte, bool, error) {
	var ch clientHello
	err := struc.Unpack(stream, &ch)
	if err != nil {
		return nil, false, err
	}
	// Speed
	if ch.Rate.SendBPS == 0 || ch.Rate.RecvBPS == 0 {
		return nil, false, errors.New("invalid rate from client")
	}
	serverSendBPS, serverRecvBPS := ch.Rate.RecvBPS, ch.Rate.SendBPS
	if s.sendBPS > 0 && serverSendBPS > s.sendBPS {
		serverSendBPS = s.sendBPS
	}
	if s.recvBPS > 0 && serverRecvBPS > s.recvBPS {
		serverRecvBPS = s.recvBPS
	}
	// Auth
	ok, msg := s.authFunc(cs.RemoteAddr(), ch.Auth, serverSendBPS, serverRecvBPS)
	// Response
	err = struc.Pack(stream, &serverHello{
		OK: ok,
		Rate: transmissionRate{
			SendBPS: serverSendBPS,
			RecvBPS: serverRecvBPS,
		},
		Message: msg,
	})
	if err != nil {
		return nil, false, err
	}
	// Set the congestion accordingly
	if ok && s.congestionFactory != nil {
		cs.SetCongestionControl(s.congestionFactory(serverSendBPS))
	}
	return ch.Auth, ok, nil
}

func (s *Server) handleStream(remoteAddr net.Addr, auth []byte, stream quic.Stream) {
	defer stream.Close()
	// Read request
	var req clientRequest
	err := struc.Unpack(stream, &req)
	if err != nil {
		return
	}
	if !req.UDP {
		// TCP connection
		s.handleTCP(remoteAddr, auth, stream, req.Address)
	} else {
		// UDP connection
		// TODO
	}
}

func (s *Server) handleTCP(remoteAddr net.Addr, auth []byte, stream quic.Stream, reqAddr string) {
	host, port, err := net.SplitHostPort(reqAddr)
	if err != nil {
		_ = struc.Pack(stream, &serverResponse{
			OK:      false,
			Message: "invalid address",
		})
		s.tcpErrorFunc(remoteAddr, auth, reqAddr, err)
		return
	}
	ip := net.ParseIP(host)
	if ip != nil {
		// IP request, clear host for ACL engine
		host = ""
	}
	action, arg := acl.ActionDirect, ""
	if s.aclEngine != nil {
		action, arg = s.aclEngine.Lookup(host, ip)
	}
	s.tcpRequestFunc(remoteAddr, auth, reqAddr, action, arg)

	var conn net.Conn // Connection to be piped
	switch action {
	case acl.ActionDirect, acl.ActionProxy: // Treat proxy as direct on server side
		conn, err = net.DialTimeout("tcp", reqAddr, dialTimeout)
		if err != nil {
			_ = struc.Pack(stream, &serverResponse{
				OK:      false,
				Message: err.Error(),
			})
			s.tcpErrorFunc(remoteAddr, auth, reqAddr, err)
			return
		}
	case acl.ActionBlock:
		_ = struc.Pack(stream, &serverResponse{
			OK:      false,
			Message: "blocked by ACL",
		})
		return
	case acl.ActionHijack:
		hijackAddr := net.JoinHostPort(arg, port)
		conn, err = net.DialTimeout("tcp", hijackAddr, dialTimeout)
		if err != nil {
			_ = struc.Pack(stream, &serverResponse{
				OK:      false,
				Message: err.Error(),
			})
			s.tcpErrorFunc(remoteAddr, auth, reqAddr, err)
			return
		}
	default:
		_ = struc.Pack(stream, &serverResponse{
			OK:      false,
			Message: "ACL error",
		})
		return
	}
	// So far so good if we reach here
	defer conn.Close()
	err = struc.Pack(stream, &serverResponse{
		OK: true,
	})
	if err != nil {
		return
	}
	err = utils.Pipe2Way(stream, conn)
	s.tcpErrorFunc(remoteAddr, auth, reqAddr, err)
}
