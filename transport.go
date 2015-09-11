// Copyright 2015 The Go Authors.
// See https://go.googlesource.com/go/+/master/CONTRIBUTORS
// Licensed under the same terms as Go itself:
// https://go.googlesource.com/go/+/master/LICENSE

package http2

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/phuslu/http2/hpack"
)

type Transport struct {
	Fallback http.RoundTripper

	// TODO: remove this and make more general with a TLS dial hook, like http
	InsecureTLSDial bool

	// Proxy specifies a function to return a proxy for a given
	// Request. If the function returns a non-nil error, the
	// request is aborted with the provided error.
	// If Proxy is nil or returns a nil *URL, no proxy is used.
	Proxy func(*http.Request) (*url.URL, error)

	connMu sync.Mutex
	conns  map[string][]*clientConn // key is host:port
}

type clientConn struct {
	t        *Transport
	tconn    *tls.Conn
	tlsState *tls.ConnectionState
	connKey  []string // key(s) this connection is cached in, in t.conns

	readerDone chan struct{} // closed on error
	readerErr  error         // set before readerDone is closed
	hdec       *hpack.Decoder
	nextRes    *http.Response

	mu           sync.Mutex
	closed       bool
	goAway       *GoAwayFrame // if non-nil, the GoAwayFrame we received
	streams      map[uint32]*clientStream
	nextStreamID uint32
	bw           *bufio.Writer
	werr         error // first write error that has occurred
	br           *bufio.Reader
	fr           *Framer
	// Settings from peer:
	maxFrameSize         uint32
	maxConcurrentStreams uint32
	initialWindowSize    uint32
	hbuf                 bytes.Buffer // HPACK encoder writes into this
	henc                 *hpack.Encoder
}

type clientStream struct {
	ID   uint32
	resc chan resAndError
	pw   *io.PipeWriter
	pr   *io.PipeReader
}

type stickyErrWriter struct {
	w   io.Writer
	err *error
}

func (sew stickyErrWriter) Write(p []byte) (n int, err error) {
	if *sew.err != nil {
		return 0, *sew.err
	}
	n, err = sew.w.Write(p)
	*sew.err = err
	return
}

func (t *Transport) RoundTrip(req *http.Request) (res *http.Response, err error) {
	if req.URL.Scheme != "https" && t.Proxy == nil {
		if t.Fallback == nil {
			return nil, errors.New("http2: unsupported scheme and no Fallback")
		}
		return t.Fallback.RoundTrip(req)
	}

	var host, port string
	if t.Proxy == nil {
		host, port, err = net.SplitHostPort(req.URL.Host)
		if err != nil {
			host = req.URL.Host
			port = "443"
		}
	} else {
		u, err := t.Proxy(req)
		if err != nil {
			return nil, err
		}
		host, port, err = net.SplitHostPort(u.Host)
		if err != nil {
			host = u.Host
			port = "443"
		}
	}

	const maxRetryRequest int = 3
	for i := 0; i < maxRetryRequest; i++ {
		cc, err := t.getClientConn(host, port)
		if err != nil {
			return nil, err
		}
		res, err = cc.roundTrip(req)
		if shouldRetryRequest(err) && i < maxRetryRequest { // TODO: or clientconn is overloaded (too many outstanding requests)?
			continue
		}
		if err != nil {
			return nil, err
		}
		return res, nil
	}
	return nil, errors.New("http2: reach max retry request times=3")
}

func (t *Transport) Connect(req *http.Request) (conn net.Conn, err error) {
	var host, port string
	if t.Proxy == nil {
		host, port, err = net.SplitHostPort(req.URL.Host)
		if err != nil {
			host = req.URL.Host
			port = "443"
		}
	} else {
		u, err := t.Proxy(req)
		if err != nil {
			return nil, err
		}
		host, port, err = net.SplitHostPort(u.Host)
		if err != nil {
			host = u.Host
			port = "443"
		}
	}

	const maxRetryRequest int = 3
	for i := 0; i < maxRetryRequest; i++ {
		cc, err := t.getClientConn(host, port)
		if err != nil {
			return nil, err
		}
		conn, err = cc.connect(req)
		if shouldRetryRequest(err) && i < maxRetryRequest { // TODO: or clientconn is overloaded (too many outstanding requests)?
			continue
		}
		if err != nil {
			return nil, err
		}
		return conn, nil
	}
	return nil, errors.New("http2: reach max retry request times=3")
}

// CloseIdleConnections closes any connections which were previously
// connected from previous requests but are now sitting idle.
// It does not interrupt any connections currently in use.
func (t *Transport) CloseIdleConnections() {
	t.connMu.Lock()
	defer t.connMu.Unlock()
	for _, vv := range t.conns {
		for _, cc := range vv {
			cc.closeIfIdle()
		}
	}
}

var errClientConnClosed = errors.New("http2: client conn is closed")

func shouldRetryRequest(err error) bool {
	// TODO: or GOAWAY graceful shutdown stuff
	return err == errClientConnClosed
}

func (t *Transport) removeClientConn(cc *clientConn) {
	t.connMu.Lock()
	defer t.connMu.Unlock()
	for _, key := range cc.connKey {
		vv, ok := t.conns[key]
		if !ok {
			continue
		}
		newList := filterOutClientConn(vv, cc)
		if len(newList) > 0 {
			t.conns[key] = newList
		} else {
			delete(t.conns, key)
		}
	}
}

func filterOutClientConn(in []*clientConn, exclude *clientConn) []*clientConn {
	out := in[:0]
	for _, v := range in {
		if v != exclude {
			out = append(out, v)
		}
	}
	return out
}

func (t *Transport) getClientConn(host, port string) (*clientConn, error) {
	t.connMu.Lock()
	defer t.connMu.Unlock()

	key := net.JoinHostPort(host, port)

	for _, cc := range t.conns[key] {
		if cc.canTakeNewRequest() {
			return cc, nil
		}
	}
	if t.conns == nil {
		t.conns = make(map[string][]*clientConn)
	}
	cc, err := t.newClientConn(host, port, key)
	if err != nil {
		return nil, err
	}
	t.conns[key] = append(t.conns[key], cc)
	return cc, nil
}

func (t *Transport) newClientConn(host, port, key string) (*clientConn, error) {
	cfg := &tls.Config{
		ServerName:         host,
		NextProtos:         []string{NextProtoTLS},
		InsecureSkipVerify: t.InsecureTLSDial,
	}
	tconn, err := tls.Dial("tcp", host+":"+port, cfg)
	if err != nil {
		return nil, err
	}
	if err := tconn.Handshake(); err != nil {
		return nil, err
	}
	if !t.InsecureTLSDial {
		if err := tconn.VerifyHostname(cfg.ServerName); err != nil {
			return nil, err
		}
	}
	state := tconn.ConnectionState()
	if p := state.NegotiatedProtocol; p != NextProtoTLS {
		// TODO(bradfitz): fall back to Fallback
		return nil, fmt.Errorf("bad protocol: %v", p)
	}
	if !state.NegotiatedProtocolIsMutual {
		return nil, errors.New("could not negotiate protocol mutually")
	}
	if _, err := tconn.Write(clientPreface); err != nil {
		return nil, err
	}

	cc := &clientConn{
		t:                    t,
		tconn:                tconn,
		connKey:              []string{key}, // TODO: cert's validated hostnames too
		tlsState:             &state,
		readerDone:           make(chan struct{}),
		nextStreamID:         1,
		maxFrameSize:         16 << 10, // spec default
		initialWindowSize:    65535,    // spec default
		maxConcurrentStreams: 1000,     // "infinite", per spec. 1000 seems good enough.
		streams:              make(map[uint32]*clientStream),
	}
	cc.bw = bufio.NewWriter(stickyErrWriter{tconn, &cc.werr})
	cc.br = bufio.NewReader(tconn)
	cc.fr = NewFramer(cc.bw, cc.br)
	cc.henc = hpack.NewEncoder(&cc.hbuf)

	cc.fr.WriteSettings()
	// TODO: re-send more conn-level flow control tokens when server uses all these.
	cc.fr.WriteWindowUpdate(0, 1<<30) // um, 0x7fffffff doesn't work to Google? it hangs?
	cc.bw.Flush()
	if cc.werr != nil {
		return nil, cc.werr
	}

	// Read the obligatory SETTINGS frame
	f, err := cc.fr.ReadFrame()
	if err != nil {
		return nil, err
	}
	sf, ok := f.(*SettingsFrame)
	if !ok {
		return nil, fmt.Errorf("expected settings frame, got: %T", f)
	}
	cc.fr.WriteSettingsAck()
	cc.bw.Flush()

	sf.ForeachSetting(func(s Setting) error {
		switch s.ID {
		case SettingMaxFrameSize:
			cc.maxFrameSize = s.Val
		case SettingMaxConcurrentStreams:
			cc.maxConcurrentStreams = s.Val
		case SettingInitialWindowSize:
			cc.initialWindowSize = s.Val
		default:
			// TODO(bradfitz): handle more
			log.Printf("Unhandled Setting: %v", s)
		}
		return nil
	})
	// TODO: figure out henc size
	cc.hdec = hpack.NewDecoder(initialHeaderTableSize, cc.onNewHeaderField)

	go cc.readLoop()
	return cc, nil
}

func (cc *clientConn) setGoAway(f *GoAwayFrame) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.goAway = f
}

func (cc *clientConn) canTakeNewRequest() bool {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.goAway == nil &&
		int64(len(cc.streams)+1) < int64(cc.maxConcurrentStreams) &&
		cc.nextStreamID < 2147483647
}

func (cc *clientConn) closeIfIdle() {
	cc.mu.Lock()
	if len(cc.streams) > 0 {
		cc.mu.Unlock()
		return
	}
	cc.closed = true
	// TODO: do clients send GOAWAY too? maybe? Just Close:
	cc.mu.Unlock()

	cc.tconn.Close()
}

type dataFrameWriter struct {
	cc        *clientConn
	cs        *clientStream
	totalSize int64
}

func (dw dataFrameWriter) Write(p []byte) (n int, err error) {
	size := len(p)
	size64 := int64(size)
	endStream := size64 >= dw.totalSize

	if err = dw.cc.fr.WriteData(dw.cs.ID, endStream, p); err != nil {
		dw.cc.werr = err
		return 0, err
	}

	if endStream {
		if err = dw.cc.bw.Flush(); err != nil {
			dw.cc.werr = err
			return 0, err
		}
	}

	dw.totalSize -= size64

	return size, err
}

func (cc *clientConn) do(req *http.Request) resAndError {
	cc.mu.Lock()

	if cc.closed {
		cc.mu.Unlock()
		return resAndError{err: errClientConnClosed}
	}

	cs := cc.newStream()
	hasBody := req.ContentLength > 0 || req.Method == "CONNECT"

	// we send: HEADERS[+CONTINUATION] + (DATA?)
	hdrs := cc.encodeHeaders(req)
	first := true
	for len(hdrs) > 0 {
		chunk := hdrs
		if len(chunk) > int(cc.maxFrameSize) {
			chunk = chunk[:cc.maxFrameSize]
		}
		hdrs = hdrs[len(chunk):]
		endHeaders := len(hdrs) == 0
		if first {
			cc.fr.WriteHeaders(HeadersFrameParam{
				StreamID:      cs.ID,
				BlockFragment: chunk,
				EndStream:     !hasBody,
				EndHeaders:    endHeaders,
			})
			first = false
		} else {
			cc.fr.WriteContinuation(cs.ID, endHeaders, chunk)
		}
	}
	cc.bw.Flush()
	werr := cc.werr
	cc.mu.Unlock()

	if hasBody {
		go io.Copy(dataFrameWriter{cc, cs, req.ContentLength}, req.Body)
	}

	if werr != nil {
		return resAndError{err: werr}
	}

	return <-cs.resc
}

func (cc *clientConn) roundTrip(req *http.Request) (*http.Response, error) {
	re := cc.do(req)
	if re.err != nil {
		return nil, re.err
	}
	res := re.res
	if cl, ok := cc.nextRes.Header["Content-Length"]; ok && cl[0] != "0" {
		res.ContentLength, _ = strconv.ParseInt(cl[0], 10, 64)
	}
	res.Request = req
	res.TLS = cc.tlsState
	return res, nil
}

type clientDataConn struct {
	re *resAndError
}

func (dc *clientDataConn) Read(p []byte) (int, error) {
	return dc.re.res.Body.Read(p)
}

func (dc *clientDataConn) Write(p []byte) (int, error) {
	if err := dc.re.cc.fr.WriteData(dc.re.cs.ID, false, p); err != nil {
		dc.re.cc.werr = err
		return 0, err
	}
	if err := dc.re.cc.bw.Flush(); err != nil {
		dc.re.cc.werr = err
		return 0, err
	}
	return len(p), nil
}

func (dc *clientDataConn) Close() (err error) {
	err = dc.re.cc.fr.WriteRSTStream(dc.re.cs.ID, ErrCodeStreamClosed)
	dc.re.cc.werr = err
	if cs, ok := dc.re.cc.streams[dc.re.cs.ID]; ok {
		delete(dc.re.cc.streams, dc.re.cs.ID)
		if p := cs.pr; p != nil {
			p.CloseWithError(io.EOF)
		}
		cs.pw.Close()
	}
	return err
}

func (dc *clientDataConn) LocalAddr() net.Addr {
	return dc.re.cc.tconn.LocalAddr()
}

func (dc *clientDataConn) RemoteAddr() net.Addr {
	return dc.re.cc.tconn.RemoteAddr()
}

func (dc *clientDataConn) SetDeadline(t time.Time) error {
	return nil
}

func (dc *clientDataConn) SetReadDeadline(t time.Time) error {
	return nil
}

func (dc *clientDataConn) SetWriteDeadline(t time.Time) error {
	return nil
}

func (cc *clientConn) connect(req *http.Request) (net.Conn, error) {
	re := cc.do(req)
	if re.err != nil {
		return nil, re.err
	}
	return &clientDataConn{&re}, nil
}

// requires cc.mu be held.
func (cc *clientConn) encodeHeaders(req *http.Request) []byte {
	cc.hbuf.Reset()

	// TODO(bradfitz): figure out :authority-vs-Host stuff between http2 and Go
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}

	path := req.RequestURI
	if path == "" {
		path = "/"
	}

	cc.writeHeader(":authority", host) // probably not right for all sites
	cc.writeHeader(":method", req.Method)
	cc.writeHeader(":path", path)
	cc.writeHeader(":scheme", req.URL.Scheme)

	for k, vv := range req.Header {
		lowKey := strings.ToLower(k)
		if lowKey == "host" {
			continue
		}
		for _, v := range vv {
			cc.writeHeader(lowKey, v)
		}
	}
	return cc.hbuf.Bytes()
}

func (cc *clientConn) writeHeader(name, value string) {
	cc.vlogf("sending %q = %q", name, value)
	cc.henc.WriteField(hpack.HeaderField{Name: name, Value: value})
}

func (cc *clientConn) vlogf(format string, args ...interface{}) {
	if VerboseLogs {
		cc.logf(format, args...)
	}
}

func (cc *clientConn) logf(format string, args ...interface{}) {
	log.Printf(format, args...)
}

type resAndError struct {
	res *http.Response
	err error
	cc  *clientConn
	cs  *clientStream
}

// requires cc.mu be held.
func (cc *clientConn) newStream() *clientStream {
	cs := &clientStream{
		ID:   cc.nextStreamID,
		resc: make(chan resAndError, 1),
	}
	cc.nextStreamID += 2
	cc.streams[cs.ID] = cs
	return cs
}

func (cc *clientConn) streamByID(id uint32, andRemove bool) *clientStream {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cs := cc.streams[id]
	if andRemove {
		delete(cc.streams, id)
	}
	return cs
}

// runs in its own goroutine.
func (cc *clientConn) readLoop() {
	defer cc.t.removeClientConn(cc)
	defer close(cc.readerDone)

	activeRes := map[uint32]*clientStream{} // keyed by streamID
	// Close any response bodies if the server closes prematurely.
	// TODO: also do this if we've written the headers but not
	// gotten a response yet.
	defer func() {
		err := cc.readerErr
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		for _, cs := range activeRes {
			cs.pw.CloseWithError(err)
		}
	}()

	// continueStreamID is the stream ID we're waiting for
	// continuation frames for.
	var continueStreamID uint32

	for {
		f, err := cc.fr.ReadFrame()
		if err != nil {
			cc.readerErr = err
			return
		}
		cc.vlogf("Transport received %v: %#v", f.Header(), f)

		streamID := f.Header().StreamID

		_, isContinue := f.(*ContinuationFrame)
		if isContinue {
			if streamID != continueStreamID {
				cc.vlogf("Protocol violation: got CONTINUATION with id %d; want %d", streamID, continueStreamID)
				cc.readerErr = ConnectionError(ErrCodeProtocol)
				return
			}
		} else if continueStreamID != 0 {
			// Continue frames need to be adjacent in the stream
			// and we were in the middle of headers.
			cc.vlogf("Protocol violation: got %T for stream %d, want CONTINUATION for %d", f, streamID, continueStreamID)
			cc.readerErr = ConnectionError(ErrCodeProtocol)
			return
		}

		if streamID%2 == 0 {
			// Ignore streams pushed from the server for now.
			// These always have an even stream id.
			continue
		}
		streamEnded := false
		if ff, ok := f.(streamEnder); ok {
			streamEnded = ff.StreamEnded()
		}

		cs := cc.streamByID(streamID, streamEnded)
		if cs == nil {
			cc.vlogf("Received frame for untracked stream ID %d", streamID)
			continue
		}

		switch f := f.(type) {
		case *HeadersFrame:
			cc.nextRes = &http.Response{
				Proto:      "HTTP/2.0",
				ProtoMajor: 2,
				Header:     make(http.Header),
			}
			cs.pr, cs.pw = io.Pipe()
			cc.hdec.Write(f.HeaderBlockFragment())
		case *ContinuationFrame:
			cc.hdec.Write(f.HeaderBlockFragment())
		case *DataFrame:
			cc.vlogf("DATA: %q", f.Data())
			cs.pw.Write(f.Data())
		case *GoAwayFrame:
			cc.t.removeClientConn(cc)
			if f.ErrCode != 0 {
				// TODO: deal with GOAWAY more. particularly the error code
				cc.vlogf("transport got GOAWAY with error code = %v", f.ErrCode)
			}
			cc.setGoAway(f)
		default:
			cc.vlogf("Transport: unhandled response frame type %T", f)
		}
		headersEnded := false
		if he, ok := f.(headersEnder); ok {
			headersEnded = he.HeadersEnded()
			if headersEnded {
				continueStreamID = 0
			} else {
				continueStreamID = streamID
			}
		}

		if streamEnded {
			cs.pw.Close()
			delete(activeRes, streamID)
		}
		if headersEnded {
			if cs == nil {
				panic("couldn't find stream") // TODO be graceful
			}
			// TODO: set the Body to one which notes the
			// Close and also sends the server a
			// RST_STREAM
			cc.nextRes.Body = cs.pr
			res := cc.nextRes
			activeRes[streamID] = cs
			cs.resc <- resAndError{res: res, cc: cc, cs: cs}
		}
	}
}

func (cc *clientConn) onNewHeaderField(f hpack.HeaderField) {
	// TODO: verifiy pseudo headers come before non-pseudo headers
	// TODO: verifiy the status is set
	cc.vlogf("Header field: %+v", f)
	if f.Name == ":status" {
		code, err := strconv.Atoi(f.Value)
		if err != nil {
			panic("TODO: be graceful")
		}
		cc.nextRes.Status = f.Value + " " + http.StatusText(code)
		cc.nextRes.StatusCode = code
		return
	}
	if strings.HasPrefix(f.Name, ":") {
		// "Endpoints MUST NOT generate pseudo-header fields other than those defined in this document."
		// TODO: treat as invalid?
		return
	}
	cc.nextRes.Header.Add(http.CanonicalHeaderKey(f.Name), f.Value)
}
