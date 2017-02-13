package httpserver

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// tlsHandler is a http.Handler that will inject a value
// into the request context indicating if the TLS
// connection is likely being intercepted.
type tlsHandler struct {
	next     http.Handler
	listener *tlsHelloListener
}

// ServeHTTP checks the User-Agent. For the four main browsers (Chrome,
// Edge, Firefox, and Safari) indicated by the User-Agent, the properties
// of the TLS Client Hello will be compared. The context value "mitm" will
// be set to a value indicating if it is likely that the underlying TLS
// connection is being intercepted.
//
// Note that due to Microsoft's decision to intentionally make IE/Edge
// user agents obscure (and look like other browsers), this may offer
// less accuracy for IE/Edge clients.
//
// This MITM detection capability is based on research done by Durumeric,
// Halderman, et. al. in "The Security Impact of HTTPS Interception" (NDSS '17):
// https://jhalderm.com/pub/papers/interception-ndss17.pdf
func (h *tlsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ua := r.Header.Get("User-Agent")
	if strings.Contains(ua, "Edge") {
		h.listener.helloInfosMu.RLock()
		info := h.listener.helloInfos[r.RemoteAddr]
		h.listener.helloInfosMu.RUnlock()
		if info.advertisesHeartbeatSupport() || !info.looksLikeEdge() {
			r = r.WithContext(context.WithValue(r.Context(), CtxKey("mitm"), true))
		} else {
			r = r.WithContext(context.WithValue(r.Context(), CtxKey("mitm"), false))
		}
	} else if strings.Contains(ua, "Chrome") {
		h.listener.helloInfosMu.RLock()
		info := h.listener.helloInfos[r.RemoteAddr]
		h.listener.helloInfosMu.RUnlock()
		if info.advertisesHeartbeatSupport() || !info.looksLikeChrome() {
			r = r.WithContext(context.WithValue(r.Context(), CtxKey("mitm"), true))
		} else {
			r = r.WithContext(context.WithValue(r.Context(), CtxKey("mitm"), false))
		}
	} else if strings.Contains(ua, "Firefox") {
		h.listener.helloInfosMu.RLock()
		info := h.listener.helloInfos[r.RemoteAddr]
		h.listener.helloInfosMu.RUnlock()
		if info.advertisesHeartbeatSupport() || !info.looksLikeFirefox() {
			r = r.WithContext(context.WithValue(r.Context(), CtxKey("mitm"), true))
		} else {
			r = r.WithContext(context.WithValue(r.Context(), CtxKey("mitm"), false))
		}
	} else if strings.Contains(ua, "Safari") {
		h.listener.helloInfosMu.RLock()
		info := h.listener.helloInfos[r.RemoteAddr]
		h.listener.helloInfosMu.RUnlock()
		if info.advertisesHeartbeatSupport() || !info.looksLikeSafari() {
			r = r.WithContext(context.WithValue(r.Context(), CtxKey("mitm"), true))
		} else {
			r = r.WithContext(context.WithValue(r.Context(), CtxKey("mitm"), false))
		}
	}
	h.next.ServeHTTP(w, r)
}

// multiConn is a net.Conn that reads from the
// given reader instead of the wire directly. This
// is useful when some of the connection has already
// been read (like the TLS Client Hello) and the
// reader is a io.MultiReader that starts with
// the contents of the buffer.
type multiConn struct {
	net.Conn
	reader io.Reader
}

// Read reads from mc.reader.
func (mc multiConn) Read(b []byte) (n int, err error) {
	return mc.reader.Read(b)
}

// parseRawClientHello parses data which contains the raw
// TLS Client Hello message. It extracts relevant information
// into info. Any error reading the Client Hello (such as
// insufficient length or invalid length values) results in
// a silent error and an incomplete info struct, since there
// is no good way to handle an error like this during Accept().
//
// The majority of this code is borrowed from the Go standard
// library, which is (c) The Go Authors. It has been modified
// to fit this use case.
func parseRawClientHello(data []byte) (info rawHelloInfo) {
	if len(data) < 42 {
		return
	}
	sessionIdLen := int(data[38])
	if sessionIdLen > 32 || len(data) < 39+sessionIdLen {
		return
	}
	data = data[39+sessionIdLen:]
	if len(data) < 2 {
		return
	}
	// cipherSuiteLen is the number of bytes of cipher suite numbers. Since
	// they are uint16s, the number must be even.
	cipherSuiteLen := int(data[0])<<8 | int(data[1])
	if cipherSuiteLen%2 == 1 || len(data) < 2+cipherSuiteLen {
		return
	}
	numCipherSuites := cipherSuiteLen / 2
	// read in the cipher suites
	info.cipherSuites = make([]uint16, numCipherSuites)
	for i := 0; i < numCipherSuites; i++ {
		info.cipherSuites[i] = uint16(data[2+2*i])<<8 | uint16(data[3+2*i])
	}
	data = data[2+cipherSuiteLen:]
	if len(data) < 1 {
		return
	}
	// read in the compression methods
	compressionMethodsLen := int(data[0])
	if len(data) < 1+compressionMethodsLen {
		return
	}
	info.compressionMethods = data[1 : 1+compressionMethodsLen]

	data = data[1+compressionMethodsLen:]

	// ClientHello is optionally followed by extension data
	if len(data) < 2 {
		return
	}
	extensionsLength := int(data[0])<<8 | int(data[1])
	data = data[2:]
	if extensionsLength != len(data) {
		return
	}

	// read in each extension, and extract any relevant information
	// from extensions we care about
	for len(data) != 0 {
		if len(data) < 4 {
			return
		}
		extension := uint16(data[0])<<8 | uint16(data[1])
		length := int(data[2])<<8 | int(data[3])
		data = data[4:]
		if len(data) < length {
			return
		}

		// record that the client advertised support for this extension
		info.extensions = append(info.extensions, extension)

		switch extension {
		case extensionSupportedCurves:
			// http://tools.ietf.org/html/rfc4492#section-5.5.1
			if length < 2 {
				return
			}
			l := int(data[0])<<8 | int(data[1])
			if l%2 == 1 || length != l+2 {
				return
			}
			numCurves := l / 2
			info.curves = make([]tls.CurveID, numCurves)
			d := data[2:]
			for i := 0; i < numCurves; i++ {
				info.curves[i] = tls.CurveID(d[0])<<8 | tls.CurveID(d[1])
				d = d[2:]
			}
		case extensionSupportedPoints:
			// http://tools.ietf.org/html/rfc4492#section-5.5.2
			if length < 1 {
				return
			}
			l := int(data[0])
			if length != l+1 {
				return
			}
			info.points = make([]uint8, l)
			copy(info.points, data[1:])
		}

		data = data[length:]
	}

	return
}

// newTLSListener returns a new tlsHelloListener that wraps ln.
func newTLSListener(ln net.Listener, config *tls.Config, readTimeout time.Duration) *tlsHelloListener {
	return &tlsHelloListener{
		Listener:    ln,
		config:      config,
		readTimeout: readTimeout,
		helloInfos:  make(map[string]rawHelloInfo),
	}
}

// tlsHelloListener is a TLS listener that is specially designed
// to read the ClientHello manually so we can extract necessary
// information from it. Each ClientHello message is mapped by
// the remote address of the client, which must be removed when
// the connection is closed (use ConnState).
type tlsHelloListener struct {
	net.Listener
	config       *tls.Config
	readTimeout  time.Duration
	helloInfos   map[string]rawHelloInfo
	helloInfosMu sync.RWMutex
}

// Accept waits for and returns the next connection to the listener.
// After it accepts the underlying connection, it reads the
// ClientHello message and stores the parsed data into a map on l.
func (l *tlsHelloListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}

	// TODO: Reading from this connection in the same goroutine is blocking, is it not?

	// Be careful to limit the amount of time to allow reading from this connection.
	conn.SetDeadline(time.Now().Add(l.readTimeout))

	// Read the header bytes.
	hdr := make([]byte, 5)
	_, err = io.ReadFull(conn, hdr)
	if err != nil {
		// returning an error will terminate the Accept loop
		// in net/http, which isn't what we want; we'll just
		// let the error occur naturally when it tries to read.
		return conn, nil
	}

	// Get the length of the ClientHello message and read it as well.
	length := uint16(hdr[3])<<8 | uint16(hdr[4])
	hello := make([]byte, int(length))
	_, err = io.ReadFull(conn, hello)
	if err != nil {
		return conn, nil
	}

	// Parse the ClientHello and store it in the map.
	rawParsed := parseRawClientHello(hello)
	l.helloInfosMu.Lock()
	l.helloInfos[conn.RemoteAddr().String()] = rawParsed
	l.helloInfosMu.Unlock()

	// Since we buffered the header and ClientHello, pretend we were
	// never here by lining up the buffered values to be read with a
	// custom connection type, followed by the rest of the actual
	// underlying connection.
	mr := io.MultiReader(bytes.NewReader(hdr), bytes.NewReader(hello), conn)
	mc := multiConn{Conn: conn, reader: mr}

	// Clear the read timeout and let the built-in TLS server take care of
	// it. This may not be a perfect way to do timeouts, but meh, it works.
	conn.SetDeadline(time.Time{})

	// Let the built-in TLS server handle the connection now as usual.
	return tls.Server(mc, l.config), nil
}

// rawHelloInfo contains the "raw" data parsed from the TLS
// Client Hello. No interpretation is done on the raw data.
//
// The methods on this type implement heuristics described
// by Durumeric, Halderman, et. al. in
// "The Security Impact of HTTPS Interception":
// https://jhalderm.com/pub/papers/interception-ndss17.pdf
type rawHelloInfo struct {
	cipherSuites       []uint16
	extensions         []uint16
	compressionMethods []byte
	curves             []tls.CurveID
	points             []uint8
}

// advertisesHeartbeatSupport returns true if info indicates
// that the client supports the Heartbeat extension.
func (info rawHelloInfo) advertisesHeartbeatSupport() bool {
	for _, ext := range info.extensions {
		if ext == extensionHeartbeat {
			return true
		}
	}
	return false
}

// looksLikeFirefox returns true if info looks like a handshake
// from a modern version of Firefox.
func (info rawHelloInfo) looksLikeFirefox() bool {
	// "To determine whether a Firefox session has been
	// intercepted, we check for the presence and order
	// of extensions, cipher suites, elliptic curves,
	// EC point formats, and handshake compression methods."

	// We check for both the presence of extensions and their ordering.
	// Note: Firefox will sometimes have 21 (padding) as first extension,
	// and other times it will not have it at all (Feb. 2017).
	if len(info.extensions) > 0 && info.extensions[0] == 21 {
		info.extensions = info.extensions[1:]
	}
	expectedExtensions := []uint16{0, 23, 65281, 10, 11, 35, 16, 5, 65283, 13}
	if len(info.extensions) != len(expectedExtensions) {
		return false
	}
	for i := range expectedExtensions {
		if info.extensions[i] != expectedExtensions[i] {
			return false
		}
	}

	// We check for both presence of curves and their ordering.
	expectedCurves := []tls.CurveID{29, 23, 24, 25}
	if len(info.curves) != len(expectedCurves) {
		return false
	}
	for i := range expectedCurves {
		if info.curves[i] != expectedCurves[i] {
			return false
		}
	}

	// We check for order of cipher suites but not presence, since
	// according to the paper, cipher suites may be not be added
	// or reordered by the user, but they may be disabled.
	expectedCipherSuiteOrder := []uint16{
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		0xc02f, // tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
		0xcca9, // tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
		tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
		tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
		0x33, // tls.TLS_DHE_RSA_WITH_AES_128_CBC_SHA,
		0x39, // tls.TLS_DHE_RSA_WITH_AES_256_CBC_SHA,
		tls.TLS_RSA_WITH_AES_128_CBC_SHA,
		tls.TLS_RSA_WITH_AES_256_CBC_SHA,
		tls.TLS_RSA_WITH_3DES_EDE_CBC_SHA,
	}
	// this loop checks the order of cipher suites
	// but tolerates missing ones
	var j int
	for _, cipherSuite := range info.cipherSuites {
		var found bool
		for j < len(expectedCipherSuiteOrder) {
			if expectedCipherSuiteOrder[j] == cipherSuite {
				found = true
				break
			}
			j++
		}
		if j == len(expectedCipherSuiteOrder)-1 && !found {
			return false
		}
	}

	return true
}

// looksLikeChrome returns true if info looks like a handshake
// from a modern version of Chrome.
func (info rawHelloInfo) looksLikeChrome() bool {
	// "We check for ciphers and extensions that Chrome is known
	// to not support, but do not check for the inclusion of
	// specific ciphers or extensions, nor do we validate their
	// order. When appropriate, we check the presence and order
	// of elliptic curves, compression methods, and EC point formats."

	// Not in Chrome 56, but present in Safari 10 (Feb. 2017):
	// TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA384 (0xc024)
	// TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA256 (0xc023)
	// TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA (0xc00a)
	// TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA (0xc009)
	// TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA384 (0xc028)
	// TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256 (0xc027)
	// TLS_RSA_WITH_AES_256_CBC_SHA256 (0x3d)
	// TLS_RSA_WITH_AES_128_CBC_SHA256 (0x3c)

	// Not in Chrome 56, but present in Firefox 51 (Feb. 2017):
	// TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA (0xc00a)
	// TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA (0xc009)
	// TLS_DHE_RSA_WITH_AES_128_CBC_SHA (0x33)
	// TLS_DHE_RSA_WITH_AES_256_CBC_SHA (0x39)

	chromeCipherExclusions := map[uint16]struct{}{
		0xc024: {}, // TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA384
		0xc023: {}, // TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA256
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA: {},
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA: {},
		0xc028: {}, // TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA384
		tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256: {},
		0x3d: {}, // TLS_RSA_WITH_AES_256_CBC_SHA256
		tls.TLS_RSA_WITH_AES_128_CBC_SHA256: {},
		0x33: {}, // TLS_DHE_RSA_WITH_AES_128_CBC_SHA
		0x39: {}, // TLS_DHE_RSA_WITH_AES_256_CBC_SHA
	}
	for _, ext := range info.cipherSuites {
		if _, ok := chromeCipherExclusions[ext]; ok {
			return false
		}
	}

	// Chrome does not include curve 25 (CurveP521).
	for _, curve := range info.curves {
		if curve == 25 {
			return false
		}
	}

	return true
}

// looksLikeEdge returns true if info looks like a handshake
// from a modern version of MS Edge.
func (info rawHelloInfo) looksLikeEdge() bool {
	// "SChannel connections can by uniquely identified because SChannel
	// is the only TLS library we tested that includes the OCSP status
	// request extension before the supported groups and EC point formats
	// extensions."
	// NOTE - TODO: Chrome also puts 5 before 10 and 11...
	var extPosOCSPStatusRequest, extPosSupportedGroups, extPosPointFormats int
	for i, ext := range info.extensions {
		switch ext {
		case extensionOCSPStatusRequest:
			extPosOCSPStatusRequest = i
		case extensionSupportedCurves:
			extPosSupportedGroups = i
		case extensionSupportedPoints:
			extPosPointFormats = i
		}
	}
	return extPosOCSPStatusRequest < extPosSupportedGroups &&
		extPosOCSPStatusRequest < extPosPointFormats
}

// looksLikeSafari returns true if info looks like a handshake
// from a modern version of MS Safari.
func (info rawHelloInfo) looksLikeSafari() bool {
	// "One unique aspect of Secure Transport is that it includes
	// the TLS_EMPTY_RENEGOTIATION_INFO_SCSV (0xff) cipher first,
	// whereas the other libraries we investigated include the
	// cipher last. Similar to Microsoft, Apple has changed
	// TLS behavior in minor OS updates, which are not indicated
	// in the HTTP User-Agent header. We allow for any of the
	// updates when validating handshakes, and we check for the
	// presence and ordering of ciphers, extensions, elliptic
	// curves, and compression methods."

	// Note that any C lib (e.g. curl) compiled on macOS
	// will probably use Secure Transport which will also
	// share the TLS handshake characteristics of Safari.

	if len(info.cipherSuites) < 1 {
		return false
	}
	return info.cipherSuites[0] == scsvRenegotiation
	// TODO: Implement checking of presence and ordering
	// of cipher suites etc. as described by the paper.
}

const (
	extensionOCSPStatusRequest = 5
	extensionSupportedCurves   = 10 // also called "SupportedGroups"
	extensionSupportedPoints   = 11
	extensionHeartbeat         = 15

	scsvRenegotiation = 0xff
)
