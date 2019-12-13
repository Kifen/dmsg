package dmsg

import (
	"context"
	"net"
	"time"

	"github.com/SkycoinProject/yamux"
	"github.com/sirupsen/logrus"

	"github.com/SkycoinProject/dmsg/noise"
)

// Stream represents a dmsg connection between two dmsg clients.
type Stream struct {
	ses  *ClientSession // back reference
	yStr *yamux.Stream

	// The following fields are to be filled after handshake.
	lAddr  Addr
	rAddr  Addr
	ns     *noise.Noise
	nsConn *noise.ReadWriter
	close  func() // to be called when closing
	log    logrus.FieldLogger
}

func newInitiatingStream(cSes *ClientSession) (*Stream, error) {
	yStr, err := cSes.ys.OpenStream()
	if err != nil {
		return nil, err
	}
	return &Stream{ses: cSes, yStr: yStr}, nil
}

func newRespondingStream(cSes *ClientSession) (*Stream, error) {
	yStr, err := cSes.ys.AcceptStream()
	if err != nil {
		return nil, err
	}
	return &Stream{ses: cSes, yStr: yStr}, nil
}

// Close closes the dmsg stream.
func (s *Stream) Close() error {
	if s == nil {
		return nil
	}
	if s.close != nil {
		s.close()
	}
	return s.yStr.Close()
}

func (s *Stream) writeRequest(rAddr Addr) (req StreamDialRequest, err error) {
	// Reserve stream in porter.
	var lPort uint16
	if lPort, s.close, err = s.ses.porter.ReserveEphemeral(context.Background(), s); err != nil {
		return
	}

	// Prepare fields.
	s.prepareFields(true, Addr{PK: s.ses.LocalPK(), Port: lPort}, rAddr)

	// Prepare request.
	var nsMsg []byte
	if nsMsg, err = s.ns.MakeHandshakeMessage(); err != nil {
		return
	}
	req = StreamDialRequest{
		Timestamp: time.Now().UnixNano(),
		SrcAddr:   s.lAddr,
		DstAddr:   s.rAddr,
		NoiseMsg:  nsMsg,
	}
	req.Sign(s.ses.localSK())

	// Write request.
	err = s.ses.writeEncryptedGob(s.yStr, req)
	return
}

func (s *Stream) readRequest() (req StreamDialRequest, err error) {
	if err = s.ses.readEncryptedGob(s.yStr, &req); err != nil {
		return
	}
	if err = req.Verify(0); err != nil {
		err = ErrReqInvalidTimestamp
		return
	}
	if req.DstAddr.PK != s.ses.LocalPK() {
		err = ErrReqInvalidDstPK
		return
	}

	// Prepare fields.
	s.prepareFields(false, req.DstAddr, req.SrcAddr)

	if err = s.ns.ProcessHandshakeMessage(req.NoiseMsg); err != nil {
		return
	}
	return
}

func (s *Stream) writeResponse(req StreamDialRequest) error {
	// Obtain associated local listener.
	pVal, ok := s.ses.porter.PortValue(s.lAddr.Port)
	if !ok {
		return ErrReqNoListener
	}
	lis, ok := pVal.(*Listener)
	if !ok {
		return ErrReqNoListener
	}

	// Prepare and write response.
	nsMsg, err := s.ns.MakeHandshakeMessage()
	if err != nil {
		return err
	}
	resp := StreamDialResponse{
		ReqHash:  req.Hash(),
		Accepted: true,
		NoiseMsg: nsMsg,
	}
	resp.Sign(s.ses.localSK())
	if err := s.ses.writeEncryptedGob(s.yStr, resp); err != nil {
		return err
	}

	// Push stream to listener.
	return lis.introduceStream(s)
}

func (s *Stream) readResponse(req StreamDialRequest) error {
	// Read and process response.
	var resp StreamDialResponse
	if err := s.ses.readEncryptedGob(s.yStr, &resp); err != nil {
		return err
	}
	if err := resp.Verify(req.DstAddr.PK, req.Hash()); err != nil {
		return err
	}
	return s.ns.ProcessHandshakeMessage(resp.NoiseMsg)
}

func (s *Stream) prepareFields(init bool, lAddr, rAddr Addr) {
	ns, err := noise.New(noise.HandshakeKK, noise.Config{
		LocalPK:   s.ses.LocalPK(),
		LocalSK:   s.ses.localSK(),
		RemotePK:  rAddr.PK,
		Initiator: init,
	})
	if err != nil {
		s.log.WithError(err).Panic("Failed to prepare stream noise object.")
	}

	s.lAddr = lAddr
	s.rAddr = rAddr
	s.ns = ns
	s.nsConn = noise.NewReadWriter(s.yStr, s.ns)
	s.log = s.ses.log.WithField("stream", s.lAddr.ShortString()+"->"+s.rAddr.ShortString())
}

// LocalAddr returns the local address of the dmsg stream.
func (s *Stream) LocalAddr() net.Addr {
	return s.lAddr
}

// RemoteAddr returns the remote address of the dmsg stream.
func (s *Stream) RemoteAddr() net.Addr {
	return s.rAddr
}

// StreamID returns the stream ID.
func (s *Stream) StreamID() uint32 {
	return s.yStr.StreamID()
}

// Read implements io.Reader
func (s *Stream) Read(b []byte) (int, error) {
	//start := time.Now()
	//s.log.WithField("start", start).Debug("begin(Read):")
	n, err := s.yStr.Read(b) // TODO(evanlinjin): Use s.nsConn
	//s.log.
	//	WithField("duration", time.Now().Sub(start)).
	//	WithField("n", n).
	//	WithField("len(b)", len(b)).
	//	WithError(err).
	//	Debug("end(Read):")
	return n, err
}

// Write implements io.Writer
func (s *Stream) Write(b []byte) (int, error) {
	//start := time.Now()
	n, err := s.yStr.Write(b) // TODO(evanlinjin): Use s.nsConn
	//s.log.
	//	WithField("duration", time.Now().Sub(start)).
	//	WithField("n", n).
	//	WithError(err).
	//	Debug("Write:")
	return n, err
}

// SetDeadline implements net.Conn
func (s *Stream) SetDeadline(t time.Time) error {
	err := s.yStr.SetDeadline(t)
	if s.log != nil && s.ns.HandshakeFinished() {
		if t.IsZero() {
			s.log.
				WithField("remaining", "zero").
				WithError(err).
				Debug("SetDeadline:")
		} else {
			s.log.
				WithField("remaining", t.Sub(time.Now())).
				WithError(err).
				Debug("SetDeadline:")
		}
	}

	return err
}

// SetReadDeadline implements net.Conn
func (s *Stream) SetReadDeadline(t time.Time) error {
	err := s.yStr.SetReadDeadline(t)
	s.log.
		WithField("remaining", t.Sub(time.Now())).
		WithError(err).
		Debug("SetReadDeadline:")
	return err
}

// SetWriteDeadline implements net.Conn
func (s *Stream) SetWriteDeadline(t time.Time) error {
	err := s.yStr.SetWriteDeadline(t)
	s.log.
		WithField("remaining", t.Sub(time.Now())).
		WithError(err).
		Debug("SetWriteDeadline:")
	return err
}
