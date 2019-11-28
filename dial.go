package websocket

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"nhooyr.io/websocket/internal/errd"
)

// DialOptions represents Dial's options.
type DialOptions struct {
	// HTTPClient is used for the connection.
	// Its Transport must return writable bodies for WebSocket handshakes.
	// http.Transport does beginning with Go 1.12.
	HTTPClient *http.Client

	// HTTPHeader specifies the HTTP headers included in the handshake request.
	HTTPHeader http.Header

	// Subprotocols lists the WebSocket subprotocols to negotiate with the server.
	Subprotocols []string

	// CompressionMode sets the compression mode.
	// See the docs on CompressionMode.
	CompressionMode CompressionMode
}

// Dial performs a WebSocket handshake on url.
//
// The response is the WebSocket handshake response from the server.
// You never need to close resp.Body yourself.
//
// If an error occurs, the returned response may be non nil.
// However, you can only read the first 1024 bytes of the body.
//
// This function requires at least Go 1.12 as it uses a new feature
// in net/http to perform WebSocket handshakes.
// See docs on the HTTPClient option and https://github.com/golang/go/issues/26937#issuecomment-415855861
func Dial(ctx context.Context, u string, opts *DialOptions) (*Conn, *http.Response, error) {
	return dial(ctx, u, opts)
}

func dial(ctx context.Context, urls string, opts *DialOptions) (_ *Conn, _ *http.Response, err error) {
	defer errd.Wrap(&err, "failed to WebSocket dial")

	if opts == nil {
		opts = &DialOptions{}
	}
	opts = &*opts
	if opts.HTTPClient == nil {
		opts.HTTPClient = http.DefaultClient
	}
	if opts.HTTPHeader == nil {
		opts.HTTPHeader = http.Header{}
	}

	secWebSocketKey, err := secWebSocketKey()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate Sec-WebSocket-Key: %w", err)
	}

	resp, err := handshakeRequest(ctx, urls, opts, secWebSocketKey)
	if err != nil {
		return nil, resp, err
	}
	respBody := resp.Body
	resp.Body = nil
	defer func() {
		if err != nil {
			// We read a bit of the body for easier debugging.
			r := io.LimitReader(respBody, 1024)
			b, _ := ioutil.ReadAll(r)
			respBody.Close()
			resp.Body = ioutil.NopCloser(bytes.NewReader(b))
		}
	}()

	copts, err := verifyServerResponse(opts, secWebSocketKey, resp)
	if err != nil {
		return nil, resp, err
	}

	rwc, ok := respBody.(io.ReadWriteCloser)
	if !ok {
		return nil, resp, fmt.Errorf("response body is not a io.ReadWriteCloser: %T", respBody)
	}

	return newConn(connConfig{
		subprotocol: resp.Header.Get("Sec-WebSocket-Protocol"),
		rwc:         rwc,
		client:      true,
		copts:       copts,
		br:          getBufioReader(rwc),
		bw:          getBufioWriter(rwc),
	}), resp, nil
}

func handshakeRequest(ctx context.Context, urls string, opts *DialOptions, secWebSocketKey string) (*http.Response, error) {
	if opts.HTTPClient.Timeout > 0 {
		return nil, errors.New("use context for cancellation instead of http.Client.Timeout; see https://github.com/nhooyr/websocket/issues/67")
	}

	u, err := url.Parse(urls)
	if err != nil {
		return nil, fmt.Errorf("failed to parse url: %w", err)
	}

	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	default:
		return nil, fmt.Errorf("unexpected url scheme: %q", u.Scheme)
	}

	req, _ := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	req.Header = opts.HTTPHeader.Clone()
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", secWebSocketKey)
	if len(opts.Subprotocols) > 0 {
		req.Header.Set("Sec-WebSocket-Protocol", strings.Join(opts.Subprotocols, ","))
	}
	if opts.CompressionMode != CompressionDisabled {
		copts := opts.CompressionMode.opts()
		copts.setHeader(req.Header)
	}

	resp, err := opts.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send handshake request: %w", err)
	}
	return resp, nil
}

func secWebSocketKey() (string, error) {
	b := make([]byte, 16)
	_, err := io.ReadFull(rand.Reader, b)
	if err != nil {
		return "", fmt.Errorf("failed to read random data from rand.Reader: %w", err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

func verifyServerResponse(opts *DialOptions, secWebSocketKey string, resp *http.Response) (*compressionOptions, error) {
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return nil, fmt.Errorf("expected handshake response status code %v but got %v", http.StatusSwitchingProtocols, resp.StatusCode)
	}

	if !headerContainsToken(resp.Header, "Connection", "Upgrade") {
		return nil, fmt.Errorf("WebSocket protocol violation: Connection header %q does not contain Upgrade", resp.Header.Get("Connection"))
	}

	if !headerContainsToken(resp.Header, "Upgrade", "WebSocket") {
		return nil, fmt.Errorf("WebSocket protocol violation: Upgrade header %q does not contain websocket", resp.Header.Get("Upgrade"))
	}

	if resp.Header.Get("Sec-WebSocket-Accept") != secWebSocketAccept(secWebSocketKey) {
		return nil, fmt.Errorf("WebSocket protocol violation: invalid Sec-WebSocket-Accept %q, key %q",
			resp.Header.Get("Sec-WebSocket-Accept"),
			secWebSocketKey,
		)
	}

	err := verifySubprotocol(opts.Subprotocols, resp)
	if err != nil {
		return nil, err
	}

	return verifyServerExtensions(resp.Header)
}

func verifySubprotocol(subprotos []string, resp *http.Response) error {
	proto := resp.Header.Get("Sec-WebSocket-Protocol")
	if proto == "" {
		return nil
	}

	for _, sp2 := range subprotos {
		if strings.EqualFold(sp2, proto) {
			return nil
		}
	}

	return fmt.Errorf("WebSocket protocol violation: unexpected Sec-WebSocket-Protocol from server: %q", proto)
}

func verifyServerExtensions(h http.Header) (*compressionOptions, error) {
	exts := websocketExtensions(h)
	if len(exts) == 0 {
		return nil, nil
	}

	ext := exts[0]
	if ext.name != "permessage-deflate" || len(exts) > 1 {
		return nil, fmt.Errorf("WebSocket protcol violation: unsupported extensions from server: %+v", exts[1:])
	}

	copts := &compressionOptions{}
	for _, p := range ext.params {
		switch p {
		case "client_no_context_takeover":
			copts.clientNoContextTakeover = true
		case "server_no_context_takeover":
			copts.serverNoContextTakeover = true
		default:
			return nil, fmt.Errorf("unsupported permessage-deflate parameter: %q", p)
		}
	}

	return copts, nil
}

var readerPool sync.Pool

func getBufioReader(r io.Reader) *bufio.Reader {
	br, ok := readerPool.Get().(*bufio.Reader)
	if !ok {
		return bufio.NewReader(r)
	}
	br.Reset(r)
	return br
}

func putBufioReader(br *bufio.Reader) {
	readerPool.Put(br)
}

var writerPool sync.Pool

func getBufioWriter(w io.Writer) *bufio.Writer {
	bw, ok := writerPool.Get().(*bufio.Writer)
	if !ok {
		return bufio.NewWriter(w)
	}
	bw.Reset(w)
	return bw
}

func putBufioWriter(bw *bufio.Writer) {
	writerPool.Put(bw)
}
