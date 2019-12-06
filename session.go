package dmsg

import (
	"bufio"
	"context"
	"errors"
	"net"

	"github.com/hashicorp/yamux"
	"github.com/sirupsen/logrus"

	"github.com/SkycoinProject/dmsg/cipher"
	"github.com/SkycoinProject/dmsg/netutil"
	"github.com/SkycoinProject/dmsg/noise"
)

// Session handles the multiplexed connection between the dmsg server and dmsg client.
type Session struct {
	lPK cipher.PubKey
	lSK cipher.SecKey
	rPK cipher.PubKey // Public key of the remote dmsg server.

	ys     *yamux.Session
	ns     *noise.Noise    // For encrypting session messages, not stream messages.
	porter *netutil.Porter // Only used by client sessions.
	getter SessionGetter   // Only used by server sessions.

	log logrus.FieldLogger
}

func InitiateSession(log logrus.FieldLogger, porter *netutil.Porter, conn net.Conn, lSK cipher.SecKey, lPK, rPK cipher.PubKey) (*Session, error) {
	ns, err := noise.New(noise.HandshakeXK, noise.Config{
		LocalPK:   lPK,
		LocalSK:   lSK,
		RemotePK:  rPK,
		Initiator: true,
	})
	if err != nil {
		return nil, err
	}

	r := bufio.NewReader(conn) // Ensure this is emptied after handshake.
	if err := noise.InitiatorHandshake(ns, r, conn); err != nil {
		return nil, err
	}
	if r.Buffered() > 0 {
		return nil, errors.New("bufio reader should be empty after session handshake")
	}

	ySes, err := yamux.Client(conn, yamux.DefaultConfig())
	if err != nil {
		return nil, err
	}
	return &Session{
		lPK:    lPK,
		lSK:    lSK,
		rPK:    rPK,
		ys:     ySes,
		ns:     ns,
		porter: porter,
		log:    log,
	}, nil
}

func RespondSession(log logrus.FieldLogger, getter SessionGetter, conn net.Conn, lSK cipher.SecKey, lPK cipher.PubKey) (*Session, error) {
	ns, err := noise.New(noise.HandshakeXK, noise.Config{
		LocalPK:   lPK,
		LocalSK:   lSK,
		Initiator: false,
	})
	if err != nil {
		return nil, err
	}

	r := bufio.NewReader(conn) // Ensure this is emptied after handshake.
	if err := noise.ResponderHandshake(ns, r, conn); err != nil {
		return nil, err
	}
	if r.Buffered() > 0 {
		return nil, errors.New("bufio reader should be empty after session handshake")
	}

	ySes, err := yamux.Server(conn, yamux.DefaultConfig())
	if err != nil {
		return nil, err
	}
	return &Session{
		lPK:    lPK,
		lSK:    lSK,
		rPK:    ns.RemoteStatic(),
		ys:     ySes,
		ns:     ns,
		getter: getter,
		log:    log,
	}, nil
}

func (s *Session) LocalPK() cipher.PubKey {
	return s.lPK
}

func (s *Session) RemotePK() cipher.PubKey {
	return s.rPK
}

func (s *Session) DialClientStream(ctx context.Context, dst Addr) (*Stream, error) {
	// Prepare yamux stream.
	ys, err := s.ys.OpenStream()
	if err != nil {
		return nil, err
	}
	// Prepare dmsg stream to reserve in porter.
	dstr := NewStream(ys, s.lSK, Addr{PK: s.lPK}, dst)
	if err := dstr.DoClientHandshake(ctx, s.log, s.porter, s.ns, dstr.ClientInitiatingHandshake); err != nil {
		return nil, err
	}
	return dstr, nil
}

func (s *Session) AcceptClientStream() error {
	ys, err := s.ys.AcceptStream()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), StreamHandshakeTimeout)
	defer cancel()

	dstr := NewStream(ys, s.lSK, Addr{PK: s.lPK}, Addr{})
	if err := dstr.DoClientHandshake(ctx, s.log, s.porter, s.ns, dstr.ClientRespondingHandshake); err != nil {
		return err
	}
	return nil
}

func (s *Session) AcceptServerStream() error {
	yStr, err := s.ys.AcceptStream()
	if err != nil {
		return err
	}
	go func() {
		err := s.handleServerStream(yStr)
		_ = yStr.Close() //nolint:errcheck
		s.log.
			WithError(err).
			Infof("AcceptServerStream stopped.")
	}()
	return nil
}

func (s *Session) handleServerStream(yStr *yamux.Stream) error {
	readRequest := func() (StreamDialRequest, error) {
		var req StreamDialRequest
		if err := readEncryptedGob(yStr, s.ns, &req); err != nil {
			return req, err
		}
		if err := req.Verify(0); err != nil { // TODO(evanlinjin): timestamp tracker.
			return req, ErrReqInvalidTimestamp
		}
		if req.SrcAddr.PK != s.rPK {
			return req, ErrReqInvalidSrcPK
		}
		return req, nil
	}

	log := s.log.WithField("fn", "handleServerStream")

	// Read request.
	req, err := readRequest()
	if err != nil {
		return err
	}
	log.Info("Request read.")

	// Obtain next session.
	log.Infof("attempting to get PK: %s", req.DstAddr.PK)
	s2, ok := s.getter(req.DstAddr.PK)
	if !ok {
		return ErrReqNoSession
	}
	log.Info("Next session obtained.")

	// Forward request and obtain/check response.
	yStr2, resp, err := s2.forwardRequest(req)
	if err != nil {
		return err
	}
	defer func() { _ = yStr2.Close() }() //nolint:errcheck

	// Forward response.
	if err := writeEncryptedGob(yStr, s.ns, resp); err != nil {
		return err
	}

	// Serve stream.
	return netutil.CopyReadWriter(yStr, yStr2)
}

func (s *Session) forwardRequest(req StreamDialRequest) (*yamux.Stream, DialResponse, error) {
	yStr, err := s.ys.OpenStream()
	if err != nil {
		return nil, DialResponse{}, err
	}
	if err := writeEncryptedGob(yStr, s.ns, req); err != nil {
		_ = yStr.Close() //nolint:errcheck
		return nil, DialResponse{}, err
	}
	var resp DialResponse
	if err := readEncryptedGob(yStr, s.ns, &resp); err != nil {
		_ = yStr.Close() //nolint:errcheck
		return nil, DialResponse{}, err
	}
	if err := resp.Verify(req.DstAddr.PK, req.Hash()); err != nil {
		_ = yStr.Close() //nolint:errcheck
		return nil, DialResponse{}, err
	}
	return yStr, resp, nil
}

func (s *Session) Close() error {
	_ = s.ys.GoAway() //nolint:errcheck

	// TODO(evanlinjin): Should this be part of dmsg.Client?
	//if s.porter != nil {
	//	s.porter.RangePortValues(func(port uint16, v interface{}) (next bool) {
	//		switch v.(type) {
	//		case *Listener:
	//			_ = v.(*Listener).Close() //nolint:errcheck
	//		case *Stream:
	//			_ = v.(*Stream).Close() //nolint:errcheck
	//		}
	//		return true
	//	})
	//}
	return s.ys.Close()
}
