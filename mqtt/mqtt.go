// Package mqtt is an MQTT client, of the protocol's versions 3.1 and
// 3.1.1, shaped for Go and not after the library that carries the bytes.
//
// A Conn is one session over one connection, and nothing more: it is
// never dialled again behind its caller's back. When the connection goes,
// the Conn is done, Err says why, and what to do about it is the caller's,
// who alone knows what it was subscribed to and what it missed. Every
// call takes a context and returns when the broker has answered it: Dial
// when the broker accepted the session, Subscribe when it granted the
// subscription, Publish when it took the message, at the quality of
// service asked for. A session keeps nothing from one Conn to the next.
//
// Messages are read, not pushed: a Subscription hands them over one at a
// time, in the order the broker sent them, to the goroutine that asks.
// A subscription nobody reads fills, and then holds back everything the
// connection receives; one left so for longer than the keep-alive loses
// the connection.
//
// The bytes are carried by Eclipse Paho, which this package alone names
// and none of whose types cross its API.
package mqtt

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	"github.com/eclipse/paho.mqtt.golang/packets"
)

// Version is a version of the protocol.
type Version uint8

// The versions spoken. The zero Version is V311.
const (
	// V31 is MQTT 3.1, whose clients name the protocol MQIsdp.
	V31 Version = 3
	// V311 is MQTT 3.1.1.
	V311 Version = 4
)

// QoS is a quality of service: how hard a message is delivered.
type QoS uint8

// The qualities of service.
const (
	// AtMostOnce sends a message once and asks for no answer.
	AtMostOnce QoS = iota
	// AtLeastOnce sends a message until the other side acknowledges it; it
	// may arrive more than once.
	AtLeastOnce
	// ExactlyOnce delivers a message once, in two exchanges.
	ExactlyOnce
)

// The defaults of Options.
const (
	DefaultKeepAlive = 30 * time.Second
	DefaultBuffer    = 16
)

// closeWait is how long Close gives what is being sent to leave.
const closeWait = 250 * time.Millisecond

// The errors, to test with errors.Is.
var (
	// ErrCredentials: the broker refused the user name or the password.
	ErrCredentials = errors.New("the broker refused the user name or password")
	// ErrNotAuthorized: the broker knows the client and does not let it
	// in.
	ErrNotAuthorized = errors.New("the broker does not authorize the client")
	// ErrClientID: the broker refused the client's identifier.
	ErrClientID = errors.New("the broker refused the client identifier")
	// ErrVersion: the broker does not speak the version asked for.
	ErrVersion = errors.New("the broker does not speak this version of the protocol")
	// ErrUnavailable: the broker answered that it is unavailable.
	ErrUnavailable = errors.New("the broker is unavailable")
	// ErrRefused: the broker refused a subscription.
	ErrRefused = errors.New("the broker refused the subscription")
	// ErrLost: the connection went, and the Conn with it. The error wraps
	// what the connection died of.
	ErrLost = errors.New("the connection is lost")
	// ErrClosed: the Conn was closed, or the Subscription unsubscribed, by
	// its owner.
	ErrClosed = errors.New("closed")
	// ErrArgument: a topic, a filter or an option that cannot be sent;
	// nothing was.
	ErrArgument = errors.New("not a valid argument")
)

// Options say where the broker is and who the client is.
//
// The password is a secret: an Options formatted, logged or marshalled as
// JSON shows what it is and not that. One kept in an unexported field of
// another struct is out of reach of these methods, and fmt prints it as it
// is. What it marshals to is for a reader and does not read back: an
// Options is made in code, not kept in a file.
type Options struct {
	// Address is the broker's host and port.
	Address string
	// TLS, when not nil, has the connection speak TLS under it.
	TLS *tls.Config
	// Version is the protocol's version; zero is V311.
	Version Version
	// ClientID names the client to the broker; empty is a random one. MQTT
	// 3.1 takes 23 bytes at most.
	ClientID string
	// Username and Password are sent when Username is not empty.
	Username string
	Password string
	// KeepAlive is the longest the connection stays silent before the
	// client pings the broker; zero is DefaultKeepAlive.
	KeepAlive time.Duration
	// Buffer is how many messages a Subscription holds unread before it
	// holds the connection back; zero is DefaultBuffer.
	Buffer int
}

// Format keeps the password out of a formatted value, whatever the verb.
func (o Options) Format(state fmt.State, _ rune) { _, _ = io.WriteString(state, o.shown()) }

// LogValue keeps it out of a log record.
func (o Options) LogValue() slog.Value { return slog.StringValue(o.shown()) }

// MarshalJSON keeps it out of JSON: an Options marshals as what it shows,
// a string.
func (o Options) MarshalJSON() ([]byte, error) {
	shown, err := json.Marshal(o.shown())
	if err != nil {
		return nil, fmt.Errorf("marshalling mqtt.Options: %w", err)
	}

	return shown, nil
}

// shown is an Options without its password.
func (o Options) shown() string {
	return fmt.Sprintf("mqtt.Options{Address:%q Version:%d ClientID:%q Username:%q TLS:%t}",
		o.Address, o.Version, o.ClientID, o.Username, o.TLS != nil)
}

// Message is one application message.
type Message struct {
	// Topic is where it is published.
	Topic string
	// Payload is its content, the receiver's own to keep.
	Payload []byte
	// QoS is the quality of service it is sent with, or was delivered
	// with.
	QoS QoS
	// Retain asks the broker to keep the message for later subscribers; on
	// a message received, it says the broker had kept it.
	Retain bool
}

// Conn is a session with a broker. Its methods may be called from several
// goroutines.
type Conn struct {
	client paho.Client
	buffer int

	done chan struct{}

	mu      sync.Mutex
	err     error
	filters map[string]struct{}
}

// Dial opens a connection to the broker and a session on it, and returns
// when the broker has accepted the session. ctx bounds the whole of that;
// it has no hold on the Conn afterwards.
func Dial(ctx context.Context, opts Options) (*Conn, error) {
	settings, err := settings(ctx, &opts)
	if err != nil {
		return nil, err
	}

	conn := &Conn{buffer: opts.Buffer, done: make(chan struct{}), filters: map[string]struct{}{}}
	if conn.buffer <= 0 {
		conn.buffer = DefaultBuffer
	}

	settings.SetConnectionLostHandler(func(_ paho.Client, cause error) {
		conn.end(fmt.Errorf("%w: %w", ErrLost, cause))
	})

	conn.client = paho.NewClient(settings)
	token := conn.client.Connect()

	select {
	case <-token.Done():
	case <-ctx.Done():
		conn.client.Disconnect(0)

		return nil, fmt.Errorf("dialling %s: %w", opts.Address, context.Cause(ctx))
	}

	if err = token.Error(); err != nil {
		return nil, fmt.Errorf("dialling %s: %w", opts.Address, refusal(err))
	}

	return conn, nil
}

// settings is the options as the carrier takes them: one clean session,
// never opened twice, its messages handed over in order.
func settings(ctx context.Context, opts *Options) (*paho.ClientOptions, error) {
	if _, _, err := net.SplitHostPort(opts.Address); err != nil {
		return nil, fmt.Errorf("%w: the broker's address %q: %w", ErrArgument, opts.Address, err)
	}

	version := opts.Version
	if version == 0 {
		version = V311
	}

	if version != V31 && version != V311 {
		return nil, fmt.Errorf("%w: version %d of the protocol", ErrArgument, version)
	}

	identifier := opts.ClientID
	if identifier == "" {
		// Sixteen hexadecimal digits, within MQTT 3.1's 23 bytes.
		random := make([]byte, 8)

		_, _ = rand.Read(random)
		identifier = "go-" + hex.EncodeToString(random)
	}

	const longestIn31 = 23
	if version == V31 && len(identifier) > longestIn31 {
		return nil, fmt.Errorf("%w: a client identifier of %d bytes, over MQTT 3.1's %d",
			ErrArgument, len(identifier), longestIn31)
	}

	keepAlive := opts.KeepAlive
	if keepAlive == 0 {
		keepAlive = DefaultKeepAlive
	}

	if keepAlive < time.Second {
		return nil, fmt.Errorf("%w: a keep-alive of %s, under a second", ErrArgument, keepAlive)
	}

	settings := paho.NewClientOptions().
		AddBroker((&url.URL{Scheme: "tcp", Host: opts.Address}).String()).
		SetProtocolVersion(uint(version)).
		SetClientID(identifier).
		SetCleanSession(true).
		SetAutoReconnect(false).
		SetConnectRetry(false).
		SetOrderMatters(true).
		SetKeepAlive(keepAlive)

	if opts.Username != "" {
		settings.SetUsername(opts.Username).SetPassword(opts.Password)
	}

	address, secured := opts.Address, opts.TLS

	// The connection is opened here, under Dial's context, and once: the
	// session is never opened again.
	settings.SetCustomOpenConnectionFn(func(*url.URL, paho.ClientOptions) (net.Conn, error) {
		var dialer net.Dialer

		raw, err := dialer.DialContext(ctx, "tcp", address)
		if err != nil || secured == nil {
			return raw, err //nolint:wrapcheck // the dialer's error says the address and the cause
		}

		over := tls.Client(raw, named(secured, address))
		if err = over.HandshakeContext(ctx); err != nil {
			_ = raw.Close()

			return nil, fmt.Errorf("TLS with %s: %w", address, err)
		}

		return over, nil
	})

	return settings, nil
}

// refusal is the error of a session the broker did not accept: one of
// this package's when the broker said why.
func refusal(err error) error {
	for refused, class := range map[error]error{
		packets.ErrorRefusedBadUsernameOrPassword: ErrCredentials,
		packets.ErrorRefusedNotAuthorised:         ErrNotAuthorized,
		packets.ErrorRefusedIDRejected:            ErrClientID,
		packets.ErrorRefusedBadProtocolVersion:    ErrVersion,
		packets.ErrorRefusedServerUnavailable:     ErrUnavailable,
	} {
		if errors.Is(err, refused) {
			return class
		}
	}

	return err
}

// Done is closed when the Conn is over: closed, or lost.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err is nil while the Conn lives, ErrClosed after Close, and an ErrLost
// with its cause when the connection went by itself.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.err
}

// Close ends the session and the connection. What is being sent is given
// a moment to leave. Closing a Conn that is over does nothing.
func (c *Conn) Close() error {
	if c.end(ErrClosed) {
		c.client.Disconnect(uint(closeWait / time.Millisecond))
	}

	return nil
}

// Publish sends a message, and returns when it has left, at AtMostOnce,
// or when the broker has acknowledged it.
func (c *Conn) Publish(ctx context.Context, message Message) error {
	if err := CheckTopic(message.Topic); err != nil {
		return err
	}

	if message.QoS > ExactlyOnce {
		return fmt.Errorf("%w: a quality of service of %d", ErrArgument, message.QoS)
	}

	if err := c.Err(); err != nil {
		return fmt.Errorf("publishing to %q: %w", message.Topic, err)
	}

	token := c.client.Publish(message.Topic, byte(message.QoS), message.Retain, message.Payload)

	return c.wait(ctx, token, fmt.Sprintf("publishing to %q", message.Topic))
}

// Subscription is the messages of the topics a filter matches. Next may
// be called from one goroutine at a time.
//
// MQTT 3 does not say which subscription a message is for. A message the
// connection receives is handed to every Subscription of the Conn whose
// filter matches its topic, each time the broker sends it; and a broker
// may send it once for each subscription it matches. Filters that overlap
// on one Conn can so receive a message more than once.
type Subscription struct {
	conn   *Conn
	filter string
	qos    QoS
	queue  chan Message

	mu     sync.Mutex
	closed chan struct{}
	over   bool
}

// Subscribe asks the broker for the messages of the topics filter
// matches, at qos at most, and returns when the broker has granted it. A
// Conn holds one subscription to a filter at a time.
func (c *Conn) Subscribe(ctx context.Context, filter string, qos QoS) (*Subscription, error) {
	if err := CheckFilter(filter); err != nil {
		return nil, err
	}

	if qos > ExactlyOnce {
		return nil, fmt.Errorf("%w: a quality of service of %d", ErrArgument, qos)
	}

	if err := c.Err(); err != nil {
		return nil, fmt.Errorf("subscribing to %q: %w", filter, err)
	}

	if !c.claim(filter) {
		return nil, fmt.Errorf("%w: %q is subscribed to already", ErrArgument, filter)
	}

	sub := &Subscription{conn: c, filter: filter, queue: make(chan Message, c.buffer), closed: make(chan struct{})}

	token := c.client.Subscribe(filter, byte(qos), sub.receive)
	what := fmt.Sprintf("subscribing to %q", filter)

	// A subscription that is not handed to the caller is nobody's to
	// read: it is ended, or its queue would fill and hold the connection
	// back for good.
	if err := c.wait(ctx, token, what); err != nil {
		sub.abandon()

		return nil, err
	}

	// The broker grants each filter a quality of service, or refuses it.
	const refused = 0x80

	if answer, ok := token.(*paho.SubscribeToken); ok {
		granted := answer.Result()[filter]
		if granted == refused {
			sub.abandon()

			return nil, fmt.Errorf("%s: %w", what, ErrRefused)
		}

		sub.qos = QoS(granted)
	}

	return sub, nil
}

// end ends the Conn, for the first cause given, and says whether it was
// this one.
func (c *Conn) end(cause error) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.err != nil {
		return false
	}

	c.err = cause

	close(c.done)

	return true
}

// wait is the outcome of something asked of the broker: its error, or
// what ended the wait first.
func (c *Conn) wait(ctx context.Context, token paho.Token, what string) error {
	select {
	case <-token.Done():
	case <-ctx.Done():
		return fmt.Errorf("%s: %w", what, context.Cause(ctx))
	case <-c.done:
		return fmt.Errorf("%s: %w", what, c.Err())
	}

	err := token.Error()
	if err == nil {
		return nil
	}

	// The carrier's own word for a Conn that is over.
	if over := c.Err(); over != nil {
		return fmt.Errorf("%s: %w", what, over)
	}

	return fmt.Errorf("%s: %w", what, err)
}

func (c *Conn) claim(filter string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, held := c.filters[filter]; held {
		return false
	}

	c.filters[filter] = struct{}{}

	return true
}

func (c *Conn) release(filter string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.filters, filter)
}

// Filter is the filter the subscription was made with.
func (s *Subscription) Filter() string { return s.filter }

// QoS is the quality of service the broker granted, which may be under
// the one asked for: messages are delivered at this one at most.
func (s *Subscription) QoS() QoS { return s.qos }

// Next is the next message, waiting for one until ctx is done. When the
// subscription or its Conn has ended, Next hands over what is queued,
// then is ErrClosed after Unsubscribe, and the Conn's Err when the Conn
// is over. A message that arrives as the subscription ends may be left
// out.
func (s *Subscription) Next(ctx context.Context) (Message, error) {
	if message, queued := s.queued(); queued {
		return message, nil
	}

	var ended error

	select {
	case message := <-s.queue:
		return message, nil
	case <-ctx.Done():
		return Message{}, fmt.Errorf("waiting on %q: %w", s.filter, context.Cause(ctx))
	case <-s.closed:
		ended = ErrClosed
	case <-s.conn.done:
		ended = s.conn.Err()
	}

	// The end and a message may have come together.
	if message, queued := s.queued(); queued {
		return message, nil
	}

	return Message{}, fmt.Errorf("waiting on %q: %w", s.filter, ended)
}

// Unsubscribe ends the subscription, and returns when the broker has
// taken it back. The subscription is over even when the broker could not
// be told. Unsubscribing twice does nothing.
func (s *Subscription) Unsubscribe(ctx context.Context) error {
	if !s.finish() {
		return nil
	}

	defer s.conn.release(s.filter)

	if s.conn.Err() != nil {
		return nil
	}

	return s.conn.wait(ctx, s.conn.client.Unsubscribe(s.filter), fmt.Sprintf("unsubscribing from %q", s.filter))
}

// finish ends the subscription for its readers and for the connection,
// and says whether it was this call that did.
func (s *Subscription) finish() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.over {
		return false
	}

	s.over = true

	close(s.closed)

	return true
}

// abandon ends a subscription the broker did not grant in time, or
// refused: nobody reads it. The broker is told to take it back, with no
// wait for its answer, in case it grants it yet.
//
// The carrier is told before the filter is given back, as in Unsubscribe:
// a filter given back first can be subscribed to again at once, and the
// unsubscription sent after it would take that new subscription away.
func (s *Subscription) abandon() {
	s.finish()

	if s.conn.Err() == nil {
		s.conn.client.Unsubscribe(s.filter)
	}

	s.conn.release(s.filter)
}

// queued is a message already received, when there is one.
func (s *Subscription) queued() (Message, bool) {
	select {
	case message := <-s.queue:
		return message, true
	default:
		return Message{}, false
	}
}

// receive takes a message from the carrier, which hands them over one
// after the other and waits for each to be taken: a full queue holds the
// connection back.
func (s *Subscription) receive(_ paho.Client, received paho.Message) {
	message := Message{
		Topic:   received.Topic(),
		Payload: append([]byte(nil), received.Payload()...),
		QoS:     QoS(received.Qos()),
		Retain:  received.Retained(),
	}

	select {
	case s.queue <- message:
	case <-s.closed:
	case <-s.conn.done:
	}
}

// named is a TLS configuration that verifies the broker under its
// address's host when it names no server and does verify: crypto/tls
// refuses one that does neither.
func named(config *tls.Config, address string) *tls.Config {
	if config.ServerName != "" || config.InsecureSkipVerify {
		return config
	}

	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return config
	}

	config = config.Clone()
	config.ServerName = host

	return config
}
