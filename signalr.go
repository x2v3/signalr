// Package signalr provides the client side implementation of the WebSocket
// portion of the SignalR protocol. This was almost entirely written using
// https://blog.3d-logic.com/2015/03/29/signalr-on-the-wire-an-informal-description-of-the-signalr-protocol/
// as a reference guide.
package signalr

import (
	"crypto/tls"
	"encoding/json"
	"io/ioutil"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/carterjones/helpers/trace"
	"github.com/carterjones/signalr/hubs"
	"github.com/gorilla/websocket"
	"github.com/pkg/errors"
)

// MessageReader is the interface that wraps ReadMessage.
//
// ReadMessage is defined at
// https://godoc.org/github.com/gorilla/websocket#Conn.ReadMessage
//
// At a high level, it reads messages and returns:
//  - the type of message read
//  - the bytes that were read
//  - any errors encountered during reading the message
type MessageReader interface {
	ReadMessage() (messageType int, p []byte, err error)
}

// JSONWriter is the interface that wraps WriteJSON.
//
// WriteJSON is defined at
// https://godoc.org/github.com/gorilla/websocket#Conn.WriteJSON
//
// At a high level, it writes a structure to the underlying websocket and
// returns any error that was encountered during the write operation.
type JSONWriter interface {
	WriteJSON(v interface{}) error
}

// WebsocketConn is a combination of MessageReader and JSONWriter. It is used to
// provide an interface to objects that can read from and write to a websocket
// connection.
type WebsocketConn interface {
	MessageReader
	JSONWriter
}

// Message represents a message sent from the server to the persistent websocket
// connection.
type Message struct {
	// message id, present for all non-KeepAlive messages
	C string

	// an array containing actual data
	M []hubs.ClientMsg

	// indicates that the transport was initialized (a.k.a. init message)
	S int

	// groups token – an encrypted string representing group membership
	G string
}

// Scheme represents a type of transport scheme. For the purposes of this
// project, we only provide constants for schemes relevant to HTTP and
// websockets.
type Scheme string

const (
	// HTTPS is the literal string, "https".
	HTTPS Scheme = "https"

	// HTTP is the literal string, "http".
	HTTP Scheme = "http"

	// WSS is the literal string, "wss".
	WSS Scheme = "wss"

	// WS is the literal string, "ws".
	WS Scheme = "ws"
)

// Client represents a SignlR client. It manages connections so that the caller
// doesn't have to.
type Client struct {
	// The host providing the SignalR service.
	Host string

	// The relative path where the SignalR service is provided.
	Endpoint string

	// The websockets protocol version.
	Protocol string

	ConnectionData string

	// The HTTPClient used to initialize the websocket connection.
	HTTPClient *http.Client

	// This field holds a struct that can read messages from and write JSON
	// objects to a websocket. In practice, this is simply a raw websocket
	// connection that results from a successful connection to the SignalR
	// server.
	Conn WebsocketConn

	// An optional setting to provide a non-default TLS configuration to use
	// when connecting to the websocket.
	TLSClientConfig *tls.Config

	// Either HTTPS or HTTP.
	Scheme Scheme

	// The maximum number of times to re-attempt a negotiation.
	MaxNegotiateRetries int

	// The maximum number of times to re-attempt a connection.
	MaxConnectRetries int

	// The maximum number of times to re-attempt a reconnection.
	MaxReconnectRetries int

	// The maximum number of times to re-attempt a start command.
	MaxStartRetries int

	// The time to wait before retrying, in the event that an error occurs
	// when contacting the SignalR service.
	RetryWaitDuration time.Duration

	// This is the connection token set during the negotiate phase of the
	// protocol and used to uniquely identify the connection to the server
	// in all subsequent phases of the connection.
	ConnectionToken string

	// This is the ID of the connection. It is set during the negotiate
	// phase and then ignored by all subsequent steps.
	ConnectionID string

	// Header values that should be applied to all HTTP requests.
	Headers map[string]string

	// This value is not part of the SignalR protocol. If this value is set,
	// it will be used in debug messages.
	CustomID string

	messages chan Message
	errs     chan error
	done     chan bool
}

func (c *Client) getCustomIDPrefix() string {
	if c.CustomID == "" {
		return ""
	}

	return "[" + c.CustomID + "] "
}

// Conditionally encrypt the traffic depending on the initial
// connection's encryption.
func (c *Client) setWebsocketURLScheme(u *url.URL) {
	if c.Scheme == HTTPS {
		u.Scheme = string(WSS)
	} else {
		u.Scheme = string(WS)
	}
}

func (c *Client) makeURL(command string) (u url.URL) {
	// Set the host.
	u.Host = c.Host

	// Set the first part of the path.
	u.Path = c.Endpoint

	// Create parameters.
	params := url.Values{}

	// Add shared parameters.
	params.Set("connectionData", c.ConnectionData)
	params.Set("clientProtocol", c.Protocol)

	// Set the connectionToken.
	if c.ConnectionToken != "" {
		params.Set("connectionToken", c.ConnectionToken)
	}

	switch command {
	case "negotiate":
		u.Scheme = string(c.Scheme)
		u.Path += "/negotiate"
	case "connect":
		c.setWebsocketURLScheme(&u)
		params.Set("transport", "webSockets")
		u.Path += "/connect"
	case "reconnect":
		c.setWebsocketURLScheme(&u)
		params.Set("transport", "webSockets")
		u.Path += "/reconnect"
	case "start":
		u.Scheme = string(c.Scheme)
		params.Set("transport", "webSockets")
		u.Path += "/start"
	}

	// Set the parameters.
	u.RawQuery = params.Encode()

	return
}

func (c *Client) prepareRequest(url string) (req *http.Request, err error) {
	req, err = http.NewRequest("GET", url, nil)
	if err != nil {
		err = errors.Wrap(err, "get request failed")
		return
	}

	// Add all header values.
	for k, v := range c.Headers {
		req.Header.Add(k, v)
	}

	return
}

func (c *Client) processNegotiateResponse(resp *http.Response, errOccurred bool) (err error) {
	var body []byte
	body, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		err = errors.Wrap(err, "read failed")
		return
	}

	// Create a struct to allow parsing of the response object.
	parsed := struct {
		URL                     string `json:"Url"`
		ConnectionToken         string
		ConnectionID            string `json:"ConnectionId"`
		KeepAliveTimeout        float64
		DisconnectTimeout       float64
		ConnectionTimeout       float64
		TryWebSockets           bool
		ProtocolVersion         string
		TransportConnectTimeout float64
		LongPollDelay           float64
	}{}
	err = json.Unmarshal(body, &parsed)
	if err != nil {
		err = errors.Wrap(err, "json unmarshal failed")
		return
	}

	if errOccurred {
		// If an error occurred earlier, and yet we got here,
		// then we want to let the user know that the
		// negotiation successfully recovered.
		trace.DebugMessage("%sthe negotiate retry was successful", c.getCustomIDPrefix())
	}

	// Set the connection token and ID.
	c.ConnectionToken = parsed.ConnectionToken
	c.ConnectionID = parsed.ConnectionID

	// Update the protocol version.
	c.Protocol = parsed.ProtocolVersion

	// Set the SignalR endpoint.
	c.Endpoint = parsed.URL

	return
}

// Negotiate implements the negotiate step of the SignalR connection sequence.
func (c *Client) Negotiate() (err error) {
	// Reset the connection token in case it has been set.
	c.ConnectionToken = ""

	// Make a "negotiate" URL.
	u := c.makeURL("negotiate")

	// Make a flag to use for indicating whether or not an error occurred.
	errOccurred := false

	for i := 0; i < c.MaxNegotiateRetries; i++ {
		var req *http.Request
		req, err = c.prepareRequest(u.String())
		if err != nil {
			err = errors.Wrap(err, "request preparation failed")
			return
		}

		// Perform the request.
		var resp *http.Response
		resp, err = c.HTTPClient.Do(req)
		if err != nil {
			err = errors.Wrap(err, "request failed")
			return
		}

		defer func() {
			derr := resp.Body.Close()
			if derr != nil {
				err = errors.Wrap(err, derr.Error())
			}
		}()

		// Perform operations specific to the status code.
		switch resp.StatusCode {
		case 200:
			// Everything worked, so do nothing.
		case 503:
			fallthrough
		case 524:
			fallthrough
		default:
			err = errors.Errorf("request failed: %s", resp.Status)
			trace.DebugMessage("%snegotiate: retrying after %s", c.getCustomIDPrefix(), resp.Status)
			errOccurred = true
			time.Sleep(c.RetryWaitDuration)
			continue
		}

		err = c.processNegotiateResponse(resp, errOccurred)
		return
	}

	if errOccurred {
		trace.DebugMessage("%sthe negotiate retry was unsuccessful", c.getCustomIDPrefix())
	}

	return
}

func (c *Client) makeHeader() (header http.Header) {
	// Create a header object that contains any cookies that have been set
	// in prior requests.
	header = make(http.Header)
	if c.HTTPClient.Jar != nil {
		// Make a negotiate URL so we can look up the cookie that was
		// set on the negotiate request.
		nu := c.makeURL("negotiate")
		cookies := ""
		for _, v := range c.HTTPClient.Jar.Cookies(&nu) {
			if cookies == "" {
				cookies += v.Name + "=" + v.Value
			} else {
				cookies += "; " + v.Name + "=" + v.Value
			}
		}

		header.Add("Cookie", cookies)
	}

	// Add all the other header values specified by the user.
	for k, v := range c.Headers {
		header.Add(k, v)
	}

	return
}

func (c *Client) xconnect(url string) (conn *websocket.Conn, err error) {
	// Create a dialer that uses the supplied TLS client configuration.
	dialer := &websocket.Dialer{
		Proxy:           http.ProxyFromEnvironment,
		TLSClientConfig: c.TLSClientConfig,
	}

	// Prepare a header to be used when dialing to the service.
	header := c.makeHeader()

	// Perform the connection in a retry loop.
	for i := 0; i < c.MaxConnectRetries; i++ {
		var resp *http.Response
		conn, resp, err = dialer.Dial(url, header)
		if err == nil {
			// If there was no error, break out of the retry loop.
			break
		}

		// According to documentation at
		// https://godoc.org/github.com/gorilla/websocket#Dialer.Dial
		// ErrBadHandshake is the only error returned. Details reside in
		// the response, so that's how we process this error.
		err = errors.Wrapf(err, "%v, retry %d", resp.Status, i)

		// Verify that a response accompanies the error.
		if resp == nil {
			// If no response is set, then wait and retry.
			time.Sleep(c.RetryWaitDuration)
			continue
		}

		// Handle any specific errors.
		switch resp.StatusCode {
		case 503:
			// Wait and retry.
			time.Sleep(c.RetryWaitDuration)
			continue
		default:
			// Return in the event that no specific error was
			// encountered.
			return
		}
	}

	return
}

// Connect implements the connect step of the SignalR connection sequence.
func (c *Client) Connect() (conn *websocket.Conn, err error) {
	// Example connect URL:
	// https://socket.bittrex.com/signalr/connect?
	//   transport=webSockets&
	//   clientProtocol=1.5&
	//   connectionToken=<token>&
	//   connectionData=%5B%7B%22name%22%3A%22corehub%22%7D%5D&
	//   tid=5
	// -> returns connection ID. (e.g.: d-F2577E41-B,0|If60z,0|If600,1)

	// Create the URL.
	u := c.makeURL("connect")

	// Perform the connection.
	conn, err = c.xconnect(u.String())
	if err != nil {
		err = errors.Wrap(err, "xconnect failed")
	}

	return
}

func (c *Client) processStartResponse(resp *http.Response, conn WebsocketConn) (err error) {
	var body []byte
	body, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		err = errors.Wrap(err, "read failed")
		return
	}

	// Create an anonymous struct to parse the response.
	parsed := struct{ Response string }{}
	err = json.Unmarshal(body, &parsed)
	if err != nil {
		err = errors.Wrap(err, "json unmarshal failed")
		return
	}

	// Confirm the server response is what we expect.
	if parsed.Response != "started" {
		err = errors.Errorf("start response is not 'started': %s", parsed.Response)
		return
	}

	// Wait for the init message.
	var t int
	var p []byte
	t, p, err = conn.ReadMessage()
	if err != nil {
		err = errors.Wrap(err, "message read failed")
		return
	}

	// Verify the correct response type was received.
	if t != websocket.TextMessage {
		err = errors.Errorf("unexpected websocket control type: %d", t)
		return
	}

	// Extract the server message.
	var pcm Message
	err = json.Unmarshal(p, &pcm)
	if err != nil {
		err = errors.Wrap(err, "json unmarshal failed")
		return
	}

	serverInitialized := 1
	if pcm.S != serverInitialized {
		err = errors.Errorf("unexpected S value received from server: %d | message: %s", pcm.S, string(p))
		return
	}

	// Since we got to this point, the connection is successful. So we set
	// the connection for the client.
	c.Conn = conn
	return
}

// Start implements the start step of the SignalR connection sequence.
func (c *Client) Start(conn WebsocketConn) (err error) {
	u := c.makeURL("start")

	var req *http.Request
	req, err = c.prepareRequest(u.String())
	if err != nil {
		err = errors.Wrap(err, "request preparation failed")
		return
	}

	// Perform the request in a retry loop.
	var resp *http.Response
	for i := 0; i < c.MaxStartRetries; i++ {
		resp, err = c.HTTPClient.Do(req)

		// Exit the retry loop if the request was successful.
		if err == nil {
			break
		}

		// If the request was unsuccessful, wrap the error, sleep, and
		// then retry.
		err = errors.Wrapf(err, "request failed (%d)", i)

		// Wait and retry.
		time.Sleep(c.RetryWaitDuration)
	}

	// If an error occurred on the last retry, then return.
	if err != nil {
		err = errors.Wrap(err, "all request retries failed")
		return
	}

	defer func() {
		derr := resp.Body.Close()
		if derr != nil {
			err = errors.Wrap(err, "close body failed")
		}
	}()

	err = c.processStartResponse(resp, conn)
	return
}

// Reconnect implements the reconnect step of the SignalR connection sequence.
func (c *Client) Reconnect() (conn *websocket.Conn, err error) {
	// Note from
	// https://blog.3d-logic.com/2015/03/29/signalr-on-the-wire-an-informal-description-of-the-signalr-protocol/
	// Once the channel is set up there are no further HTTP requests until
	// the client is stopped (the abort request) or the connection was lost
	// and the client tries to re-establish the connection (the reconnect
	// request).

	// Example reconnect URL:
	// https://socket.bittrex.com/signalr/reconnect?
	//   transport=webSockets&
	//   messageId=d-F2577E41-B%2C0%7CIf60z%2C0%7CIf600%2C1&
	//   clientProtocol=1.5&
	//   connectionToken=<same-token-as-above>&
	//   connectionData=%5B%7B%22name%22%3A%22corehub%22%7D%5D&
	//   tid=7
	// Note: messageId matches connection ID returned from the connect request

	// Create the URL.
	u := c.makeURL("reconnect")

	// Perform the reconnection.
	conn, err = c.xconnect(u.String())
	if err != nil {
		err = errors.Wrap(err, "xconnect failed")
		return
	}

	// Once complete, set the new connection for this client.
	c.Conn = conn

	return
}

// Init connects to the host and performs the websocket initialization routines
// that are part of the SignalR specification.
func (c *Client) Init() (err error) {
	err = c.Negotiate()
	if err != nil {
		err = errors.Wrap(err, "negotiate failed")
		return
	}

	var conn *websocket.Conn
	conn, err = c.Connect()
	if err != nil {
		err = errors.Wrap(err, "connect failed")
		return
	}

	err = c.Start(conn)
	if err != nil {
		err = errors.Wrap(err, "start failed")
		return
	}

	// Start the read message loop.
	go c.readMessages()

	return
}

func (c *Client) attemptReconnect() (err error) {
	// Attempt to reconnect in a retry loop.
	reconnected := false
	for i := 0; i < c.MaxReconnectRetries; i++ {
		trace.DebugMessage("%sattempting to reconnect...", c.getCustomIDPrefix())
		_, err = c.Reconnect()
		if err != nil {
			err = errors.Wrapf(err, "reconnect failed (%d)", i)
			continue
		}

		trace.DebugMessage("%sreconnected successfully", c.getCustomIDPrefix())
		reconnected = true
		break
	}

	// If the reconnect attempt succeeded, start the read message loop
	// again and then exit.
	if reconnected {
		go c.readMessages()
		return
	}

	// If the reconnect attempt failed, retry the init sequence again.
	err = c.Init()
	return
}

func errCode(err error) (code int) {
	re := regexp.MustCompile("[0-9]+")
	s := re.FindString(err.Error())

	var e error
	code, e = strconv.Atoi(s)
	if e != nil {
		// -1 is not a valid error code, so we use this, rather than
		// introducing the need for another error handler on the caller
		// of this function.
		code = -1
	}
	return
}

func (c *Client) processReadMessagesError(err error) {
	// Handle various types of errors.
	// https://tools.ietf.org/html/rfc6455#section-7.4.1
	code := errCode(err)
	switch code {
	case 1000:
		// normal closure
		fallthrough
	case 1001:
		// going away
		fallthrough
	case 1006:
		// abnormal closure
		err = c.attemptReconnect()
		if err != nil {
			err = errors.Wrap(err, "reconnect attempt failed")
			c.errs <- err
		}
	default:
		c.errs <- err
	}
}

func (c *Client) processReadMessagesMessage(p []byte) (err error) {
	// Ignore KeepAlive messages.
	if string(p) == "{}" {
		return
	}

	var msg Message
	err = json.Unmarshal(p, &msg)
	if err != nil {
		err = errors.Wrap(err, "json unmarshal failled")
		return
	}

	c.messages <- msg
	return
}

func (c *Client) readMessages() {
	for {
		// Prepare channels for the select statement later.
		pCh := make(chan []byte)
		errCh := make(chan error)

		// Wait for a message.
		go func() {
			_, p, err := c.Conn.ReadMessage()
			if err != nil {
				errCh <- err
			} else {
				pCh <- p
			}
		}()

		select {
		case err := <-errCh:
			c.processReadMessagesError(err)
			return
		case p := <-pCh:
			err := c.processReadMessagesMessage(p)
			if err != nil {
				c.errs <- err
			}
		case <-c.done:
			return
		}
	}
}

// Send sends a message to the websocket connection.
func (c *Client) Send(m hubs.ClientMsg) (err error) {
	// Verify a connection has been created.
	if c.Conn == nil {
		err = errors.New("send: connection not set")
		return
	}

	// Write the message.
	err = c.Conn.WriteJSON(m)
	if err != nil {
		err = errors.Wrap(err, "json write failed")
		return
	}

	return
}

// Messages returns the channel that receives persistent connection messages.
func (c *Client) Messages() <-chan Message {
	return c.messages
}

// Errors returns the channel that reports any errors that were encountered
// while reading from the underlying websocket connection.
func (c *Client) Errors() <-chan error {
	return c.errs
}

// Close sends a signal to shut down the connection.
func (c *Client) Close() {
	c.done <- true
}

// New creates and initializes a SignalR client.
func New(host, protocol, endpoint, connectionData string) (c *Client) {
	c = new(Client)
	c.Host = host
	c.Protocol = protocol
	c.Endpoint = endpoint
	c.ConnectionData = connectionData
	c.messages = make(chan Message)
	c.errs = make(chan error)
	c.done = make(chan bool)
	c.HTTPClient = new(http.Client)
	c.Headers = make(map[string]string)

	// Default to using a secure scheme.
	c.Scheme = HTTPS

	// Set the default max number of negotiate retries.
	c.MaxNegotiateRetries = 5

	// Set the default max number of connect retries.
	c.MaxConnectRetries = 5

	// Set the default max number of reconnect retries.
	c.MaxReconnectRetries = 5

	// Set the default max number of start retries.
	c.MaxStartRetries = 5

	// Set the default sleep duration between retries.
	c.RetryWaitDuration = 1 * time.Minute

	return
}
