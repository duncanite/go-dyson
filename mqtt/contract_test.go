package mqtt_test

// The contract is MQTT 3.1.1, the OASIS standard
// (https://docs.oasis-open.org/mqtt/mqtt/v3.1.1/os/mqtt-v3.1.1-os.html),
// and MQTT 3.1 where it differs.
//
// A session, 3.1: the client's CONNECT names the protocol, MQTT at level
// 4, or MQIsdp at level 3 for MQTT 3.1, with a client identifier, a clean
// session flag, a keep-alive in seconds, and a user name and password
// when it has them. The broker's CONNACK accepts it or refuses it with a
// return code: 1 the protocol's version, 2 the identifier, 3 unavailable,
// 4 the user name or password, 5 not authorized.
//
// Topics, 4.7: a topic or a filter is at least one character and 65535
// bytes at most, with no NUL. A topic holds no wildcard. In a filter, "+"
// is a whole level and matches any one level; "#" is the whole of the
// last level and matches that level's parent and everything below it.
// "The Server MUST NOT match Topic Filters starting with a wildcard
// character (# or +) with Topic Names beginning with a $ character."
//
// Quality of service, 4.3: at 0 a message is sent with no answer; at 1 it
// is answered by PUBACK; at 2 by PUBREC, PUBREL and PUBCOMP. A
// subscription is granted a quality of service, or refused with 0x80
// (3.9.3), and a message is delivered at the lower of its own and the
// subscription's.
//
// A retained message, 3.3.1.3: the broker keeps the last one of a topic,
// and sends it to a new subscription with its RETAIN flag set; a message
// routed to a subscription that already stood has the flag clear.

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/duncanite/go-dyson/mqtt"
	"github.com/duncanite/go-dyson/mqtt/mqtttest"
)

func started(t *testing.T) *mqtttest.Broker {
	t.Helper()

	broker := mqtttest.New()
	t.Cleanup(broker.Close)

	return broker
}

func dialled(t *testing.T, broker *mqtttest.Broker, opts mqtt.Options) *mqtt.Conn {
	t.Helper()

	opts.Address = broker.Addr()

	conn, err := mqtt.Dial(t.Context(), opts)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	t.Cleanup(func() { _ = conn.Close() })

	return conn
}

// read is the next message of a subscription; the test fails when none
// comes.
func read(ctx context.Context, t *testing.T, sub *mqtt.Subscription) mqtt.Message {
	t.Helper()

	message, err := sub.Next(ctx)
	if err != nil {
		t.Fatalf("Next on %q: %v", sub.Filter(), err)
	}

	return message
}

// soon is a context for something that must happen at once.
func soon(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)

	return ctx
}

// TestSession: the session a client opens, in each version of the
// protocol: its name and level, a clean session, the keep-alive, the
// identifier given or a random one MQTT 3.1 takes, the user name and
// password when there is a user name.
func TestSession(t *testing.T) {
	t.Parallel()

	broker := started(t)

	dialled(t, broker, mqtt.Options{})
	dialled(t, broker, mqtt.Options{
		Version: mqtt.V31, ClientID: "a-client", Username: "someone", Password: "a secret",
		KeepAlive: 90 * time.Second,
	})
	dialled(t, broker, mqtt.Options{Version: mqtt.V31, Password: "sent with no user name"})

	hellos := broker.Hellos()
	if len(hellos) != 3 {
		t.Fatalf("the broker saw %+v", hellos)
	}

	first, random := hellos[0], hellos[0].ClientID
	if first.Protocol != "MQTT" || first.Version != 4 || !first.Clean || first.KeepAlive != 30 ||
		first.Username != "" || !strings.HasPrefix(random, "go-") || len(random) != 19 {
		t.Errorf("a session with no option opened as %+v", first)
	}

	if want := (mqtttest.Hello{
		Protocol: "MQIsdp", Version: 3, ClientID: "a-client", Username: "someone", Password: "a secret",
		KeepAlive: 90, Clean: true,
	}); hellos[1] != want {
		t.Errorf("a session of MQTT 3.1 opened as %+v, want %+v", hellos[1], want)
	}

	if third := hellos[2]; third.Password != "" || third.ClientID == random || len(third.ClientID) > 23 {
		t.Errorf("a session with a password and no user name opened as %+v", third)
	}
}

// TestRefused: each way a broker refuses a session is its own error, and
// no session of the client's stands.
func TestRefused(t *testing.T) {
	t.Parallel()

	broker := started(t)

	answer := mqtttest.Accepted
	broker.Accept = func(mqtttest.Hello) mqtttest.Code { return answer }

	for code, want := range map[mqtttest.Code]error{
		mqtttest.BadVersion: mqtt.ErrVersion, mqtttest.BadClientID: mqtt.ErrClientID,
		mqtttest.Unavailable: mqtt.ErrUnavailable, mqtttest.BadCredentials: mqtt.ErrCredentials,
		mqtttest.NotAuthorized: mqtt.ErrNotAuthorized,
	} {
		answer = code

		conn, err := mqtt.Dial(t.Context(), mqtt.Options{Address: broker.Addr(), Username: "u", Password: "hunter2"})
		if conn != nil || !errors.Is(err, want) || strings.Contains(err.Error(), "hunter2") {
			t.Errorf("Dial answered %d = %v, %v; want %v", code, conn, err, want)
		}
	}
}

// TestMessages: what is published reaches who subscribed, at each quality
// of service and from the broker itself, whole and in order; a filter's
// wildcard takes the topics it matches and no other.
func TestMessages(t *testing.T) {
	t.Parallel()

	broker := started(t)
	conn := dialled(t, broker, mqtt.Options{Version: mqtt.V31})
	ctx := soon(t)

	sub, err := conn.Subscribe(ctx, "home/+/state", mqtt.ExactlyOnce)
	if err != nil || sub.Filter() != "home/+/state" || sub.QoS() != mqtt.ExactlyOnce {
		t.Fatalf("Subscribe: %v", err)
	}

	for _, qos := range []mqtt.QoS{mqtt.AtMostOnce, mqtt.AtLeastOnce, mqtt.ExactlyOnce} {
		sent := mqtt.Message{Topic: "home/fan/state", Payload: fmt.Appendf(nil, "at %d", qos), QoS: qos}

		if err = conn.Publish(ctx, sent); err != nil {
			t.Fatalf("Publish at %d: %v", qos, err)
		}

		got := read(ctx, t, sub)
		if got.Topic != sent.Topic || !bytes.Equal(got.Payload, sent.Payload) || got.QoS != qos || got.Retain {
			t.Errorf("Next after a message at %d = %+v", qos, got)
		}
	}

	// Not for this subscription: another level, and one level too many.
	for _, topic := range []string{"home/fan/speed", "home/fan/state/now"} {
		if err = conn.Publish(ctx, mqtt.Message{Topic: topic, QoS: mqtt.AtLeastOnce}); err != nil {
			t.Fatalf("Publish to %s: %v", topic, err)
		}
	}

	broker.Publish(mqtt.Message{Topic: "home/light/state", Payload: []byte("from the broker")})

	if got, err := sub.Next(
		ctx,
	); err != nil || got.Topic != "home/light/state" ||
		string(got.Payload) != "from the broker" {
		t.Errorf("Next after the broker's own message = %+v", got)
	}

	if published := broker.Published(); len(published) != 5 || published[2].QoS != mqtt.ExactlyOnce {
		t.Errorf("the broker took %+v", published)
	}
}

// TestHeldBack: a subscription that is not read loses nothing: what the
// broker sent meanwhile is there, in order, when it is read.
func TestHeldBack(t *testing.T) {
	t.Parallel()

	broker := started(t)
	conn := dialled(t, broker, mqtt.Options{Buffer: 1})
	ctx := soon(t)

	sub, err := conn.Subscribe(ctx, "count", mqtt.AtMostOnce)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	const many = 200

	for index := range many {
		broker.Publish(mqtt.Message{Topic: "count", Payload: fmt.Append(nil, index)})
	}

	for index := range many {
		if got := read(ctx, t, sub); string(got.Payload) != strconv.Itoa(index) {
			t.Fatalf("message %d read as %q", index, got.Payload)
		}
	}
}

// TestRetained: a message published to be retained is given to a later
// subscription, marked so; one that reaches a subscription that already
// stood is not marked.
func TestRetained(t *testing.T) {
	t.Parallel()

	broker := started(t)
	conn := dialled(t, broker, mqtt.Options{})
	ctx := soon(t)

	if err := conn.Publish(
		ctx,
		mqtt.Message{Topic: "kept", Payload: []byte("first"), QoS: mqtt.AtLeastOnce, Retain: true},
	); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	sub, err := conn.Subscribe(ctx, "#", mqtt.AtLeastOnce)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if got := read(ctx, t, sub); string(got.Payload) != "first" || !got.Retain {
		t.Errorf("the retained message came as %+v", got)
	}

	if err = conn.Publish(ctx, mqtt.Message{Topic: "kept", Payload: []byte("second"), Retain: true}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	if got := read(ctx, t, sub); string(got.Payload) != "second" || got.Retain {
		t.Errorf("a message to a subscription that stood came as %+v", got)
	}
}

// TestRetainedBehindTheGrant: what a broker sends right behind its grant,
// more than a subscription holds, is no overrun: the grant is read ahead
// of it, and every message waits for its reader.
func TestRetainedBehindTheGrant(t *testing.T) {
	t.Parallel()

	const kept = 50

	broker := started(t)
	conn := dialled(t, broker, mqtt.Options{Buffer: 2})
	ctx := soon(t)

	for index := range kept {
		if err := conn.Publish(ctx, mqtt.Message{
			Topic: "kept/" + strconv.Itoa(index), Payload: []byte("held"), QoS: mqtt.AtLeastOnce, Retain: true,
		}); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}

	sub, err := conn.Subscribe(ctx, "kept/#", mqtt.AtMostOnce)
	if err != nil {
		t.Fatalf("Subscribe with %d retained messages behind the grant = %v", kept, err)
	}

	topics := map[string]bool{}

	for range kept {
		topics[read(ctx, t, sub).Topic] = true
	}

	if len(topics) != kept {
		t.Errorf("%d retained messages were read on %d topics", kept, len(topics))
	}
}

// TestSubscriptions: a filter is held once on a Conn, and again once
// unsubscribed; an unsubscribed subscription hands over what it had and
// then says it is closed, and receives no more; a filter the broker
// refuses is ErrRefused.
func TestSubscriptions(t *testing.T) {
	t.Parallel()

	broker := started(t)
	broker.Refused = map[string]bool{"forbidden/#": true}

	conn := dialled(t, broker, mqtt.Options{})
	ctx := soon(t)

	sub, err := conn.Subscribe(ctx, "a/b", mqtt.AtMostOnce)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if _, err = conn.Subscribe(ctx, "a/b", mqtt.AtLeastOnce); !errors.Is(err, mqtt.ErrArgument) {
		t.Errorf("Subscribe to a filter held already = %v, want ErrArgument", err)
	}

	if _, err = conn.Subscribe(ctx, "forbidden/#", mqtt.AtMostOnce); !errors.Is(err, mqtt.ErrRefused) {
		t.Errorf("Subscribe to a filter the broker refuses = %v, want ErrRefused", err)
	}

	// A refused subscription is nobody's to read and holds nothing: what
	// a broker sends on it all the same, more than a subscription keeps,
	// does not hold the connection back; and its filter is given back,
	// to be asked for again and refused again.
	for range 2 * mqtt.DefaultBuffer {
		broker.Push(mqtt.Message{Topic: "forbidden/x", Payload: []byte("unasked")})
	}

	// Once this is read, what was sent ahead of it is in, and dropped:
	// the filter is not asked for again with messages still on their way
	// to it.
	broker.Publish(mqtt.Message{Topic: "a/b", Payload: []byte("through")})

	if got := read(ctx, t, sub); string(got.Payload) != "through" {
		t.Fatalf("after what a refused subscription was sent, the subscription held read %+v", got)
	}

	if _, err = conn.Subscribe(ctx, "forbidden/#", mqtt.AtMostOnce); !errors.Is(err, mqtt.ErrRefused) {
		t.Errorf("Subscribe again to a filter the broker refused = %v, want ErrRefused", err)
	}

	broker.Publish(mqtt.Message{Topic: "a/b", Payload: []byte("before")})

	// Once the marker is read the message before it is in: the connection
	// hands them over in order.
	other, err := conn.Subscribe(ctx, "z/+", mqtt.AtMostOnce)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	broker.Publish(mqtt.Message{Topic: "z/c", Payload: []byte("marker")})

	if got := read(ctx, t, other); string(got.Payload) != "marker" {
		t.Fatalf("Next on the other subscription = %+v", got)
	}

	if err = sub.Unsubscribe(ctx); err != nil {
		t.Fatalf("Unsubscribe: %v", err)
	}

	if err = sub.Unsubscribe(ctx); err != nil {
		t.Errorf("Unsubscribe again = %v", err)
	}

	if got := read(ctx, t, sub); string(got.Payload) != "before" {
		t.Errorf("Next after Unsubscribe, with a message in = %+v", got)
	}

	if _, err = sub.Next(ctx); !errors.Is(err, mqtt.ErrClosed) {
		t.Errorf("Next on a subscription that is over = %v, want ErrClosed", err)
	}

	if filters := broker.Filters(); len(filters) != 1 || filters[0] != "z/+" {
		t.Errorf("the broker holds %v after the unsubscription", filters)
	}

	if again, err := conn.Subscribe(ctx, "a/b", mqtt.AtMostOnce); err != nil || again == sub {
		t.Errorf("Subscribe again to a filter given back = %v", err)
	}
}

// TestOverlap: MQTT 3 does not say which subscription a message is for:
// each subscription of a Conn whose filter matches a topic is handed the
// message.
func TestOverlap(t *testing.T) {
	t.Parallel()

	broker := started(t)
	conn := dialled(t, broker, mqtt.Options{})
	ctx := soon(t)

	exact, err := conn.Subscribe(ctx, "a/b", mqtt.AtMostOnce)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	wide, err := conn.Subscribe(ctx, "a/#", mqtt.AtMostOnce)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	broker.Publish(mqtt.Message{Topic: "a/b", Payload: []byte("both")})
	broker.Publish(mqtt.Message{Topic: "a/c", Payload: []byte("wide alone")})

	for _, want := range []string{"both", "wide alone"} {
		if got := read(ctx, t, wide); string(got.Payload) != want {
			t.Errorf("the wide subscription read %q, %v; want %q", got.Payload, err, want)
		}
	}

	if got := read(ctx, t, exact); string(got.Payload) != "both" {
		t.Errorf("the exact subscription read %q", got.Payload)
	}

	brief, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()

	if got, err := exact.Next(brief); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the exact subscription read more: %+v", got)
	}
}

// TestGivenUp: a subscription whose grant did not come in time is nobody's
// to read. What the broker sent on it meanwhile, and what it sends there
// when it grants it after all, holds nothing back: the subscriptions the
// caller does hold go on receiving, the broker is told, and the filter can
// be asked for again.
func TestGivenUp(t *testing.T) {
	t.Parallel()

	broker := started(t)
	conn := dialled(t, broker, mqtt.Options{Buffer: 2})
	ctx := soon(t)

	kept, err := conn.Subscribe(ctx, "a/#", mqtt.AtMostOnce)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	grant := broker.Hold()

	// While its grant is held back the broker sends on the filter all the
	// same, as it may, and more than a subscription keeps: what nobody
	// can read yet holds nothing back.
	early := make(chan struct{})

	go func() {
		defer close(early)

		time.Sleep(20 * time.Millisecond)

		for range 64 {
			broker.Push(mqtt.Message{Topic: "b/x", Payload: []byte("early")})
		}
	}()

	brief, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()

	if _, err = conn.Subscribe(brief, "b/#", mqtt.AtMostOnce); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Subscribe with its grant held back = %v, want the deadline", err)
	}

	<-early
	grant()

	// More than the buffer holds, on the filter given up on, from a broker
	// that has not read yet that it was given up on.
	for index := range 10 {
		broker.Push(mqtt.Message{Topic: "b/x", Payload: []byte(strconv.Itoa(index))})
	}

	broker.Publish(mqtt.Message{Topic: "a/x", Payload: []byte("still here")})

	if got := read(ctx, t, kept); string(got.Payload) != "still here" {
		t.Errorf("the subscription held read %q", got.Payload)
	}

	if conn.Err() != nil {
		t.Errorf("Err after a subscription was given up on = %v", conn.Err())
	}

	// The broker was told: once it has read on, it holds the filter no
	// more.
	for deadline := time.Now().Add(5 * time.Second); slices.Contains(broker.Filters(), "b/#"); {
		if time.Now().After(deadline) {
			t.Fatalf("the broker still holds %v after a subscription was given up on", broker.Filters())
		}

		time.Sleep(time.Millisecond)
	}

	go grant()

	again, err := conn.Subscribe(ctx, "b/#", mqtt.AtMostOnce)
	if err != nil {
		t.Fatalf("Subscribe again to the filter given up on = %v", err)
	}

	broker.Publish(mqtt.Message{Topic: "b/x", Payload: []byte("anew")})

	if got := read(ctx, t, again); string(got.Payload) != "anew" {
		t.Errorf("the filter asked for again read %q", got.Payload)
	}
}

// TestEarly: what the broker sends on a filter before it grants it, when
// nobody can read yet. As many as the buffer holds are kept, and are read
// first, in order. More is ErrOverrun once the broker has answered, and no
// wait: the Conn's other subscription reads on meanwhile, the broker is
// told, and the filter can be asked for again.
func TestEarly(t *testing.T) {
	t.Parallel()

	const buffer = 2

	for _, early := range []int{0, 1, buffer, buffer + 1, 8, 64, 200} {
		t.Run(strconv.Itoa(early)+" messages", func(t *testing.T) {
			t.Parallel()

			broker := started(t)
			conn := dialled(t, broker, mqtt.Options{Buffer: buffer})
			ctx := soon(t)

			kept, err := conn.Subscribe(ctx, "a/#", mqtt.AtMostOnce)
			if err != nil {
				t.Fatalf("Subscribe: %v", err)
			}

			grant := broker.Hold()

			type answer struct {
				sub *mqtt.Subscription
				err error
			}

			asked := make(chan answer, 1)

			go func() {
				sub, failed := conn.Subscribe(ctx, "b/#", mqtt.AtMostOnce)
				asked <- answer{sub, failed}
			}()

			// The broker has the request: from here the client takes what
			// comes on the filter.
			for deadline := time.Now().Add(5 * time.Second); broker.Waiting() == 0; {
				if time.Now().After(deadline) {
					t.Fatal("the broker was never asked for the subscription")
				}

				time.Sleep(time.Millisecond)
			}

			for index := range early {
				broker.Push(mqtt.Message{Topic: "b/x", Payload: []byte(strconv.Itoa(index))})
			}

			// Messages are handed over in order: once this one is read,
			// every early one was taken, with the grant still to come.
			broker.Push(mqtt.Message{Topic: "a/x", Payload: []byte("behind them")})

			if got := read(ctx, t, kept); string(got.Payload) != "behind them" {
				t.Fatalf("behind %d early messages, the subscription held read %q", early, got.Payload)
			}

			grant()

			got := <-asked

			if early <= buffer {
				if got.err != nil {
					t.Fatalf("Subscribe with %d early messages = %v", early, got.err)
				}

				for index := range early {
					if message := read(ctx, t, got.sub); string(message.Payload) != strconv.Itoa(index) {
						t.Fatalf("early message %d read %q", index, message.Payload)
					}
				}

				broker.Publish(mqtt.Message{Topic: "b/x", Payload: []byte("later")})

				if message := read(ctx, t, got.sub); string(message.Payload) != "later" {
					t.Errorf("after the early messages, read %q", message.Payload)
				}

				return
			}

			if !errors.Is(got.err, mqtt.ErrOverrun) || got.sub != nil {
				t.Fatalf("Subscribe with %d early messages = %v, want ErrOverrun", early, got.err)
			}

			if conn.Err() != nil {
				t.Errorf("Err after a subscription was overrun = %v", conn.Err())
			}

			for deadline := time.Now().Add(5 * time.Second); slices.Contains(broker.Filters(), "b/#"); {
				if time.Now().After(deadline) {
					t.Fatalf("the broker still holds %v after a subscription was overrun", broker.Filters())
				}

				time.Sleep(time.Millisecond)
			}

			go grant()

			again, err := conn.Subscribe(ctx, "b/#", mqtt.AtMostOnce)
			if err != nil {
				t.Fatalf("Subscribe again to the filter overrun = %v", err)
			}

			broker.Publish(mqtt.Message{Topic: "b/x", Payload: []byte("anew")})

			if message := read(ctx, t, again); string(message.Payload) != "anew" {
				t.Errorf("the filter asked for again read %q", message.Payload)
			}
		})
	}
}

// TestGivenUpAndTakenAgain: a filter given up on can be asked for again
// the moment it is given back, by another goroutine, and the subscription
// that goroutine then holds is the one the broker keeps: the word that
// the first was given up on reaches the broker ahead of the new one, and
// never takes it away.
func TestGivenUpAndTakenAgain(t *testing.T) {
	t.Parallel()

	broker := started(t)
	conn := dialled(t, broker, mqtt.Options{})
	ctx := soon(t)
	grant := broker.Hold()

	for round := range 100 {
		taken := make(chan *mqtt.Subscription, 1)

		// Another goroutine asks for the filter again and again, and has
		// it the moment it is given back.
		go func() {
			for {
				sub, err := conn.Subscribe(ctx, "b/#", mqtt.AtMostOnce)
				if errors.Is(err, mqtt.ErrArgument) {
					continue
				}

				if err != nil {
					t.Errorf("round %d: Subscribe to the filter given back = %v", round, err)
				}

				taken <- sub

				return
			}
		}()

		brief, cancel := context.WithTimeout(ctx, time.Millisecond)

		_, err := conn.Subscribe(brief, "b/#", mqtt.AtMostOnce)

		cancel()

		// The other goroutine may have been the first to ask: then this
		// one was refused the filter, and there is nothing given up on.
		lost := errors.Is(err, mqtt.ErrArgument)
		if !lost && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("round %d: Subscribe with its grant held back = %v, want the deadline", round, err)
		}

		// The grant of the one given up on, then of the one taken again.
		if !lost {
			grant()
		}

		grant()

		again := <-taken
		if again == nil {
			t.FailNow()
		}

		broker.Publish(mqtt.Message{Topic: "b/x", Payload: []byte(strconv.Itoa(round))})

		if got := read(ctx, t, again); string(got.Payload) != strconv.Itoa(round) {
			t.Fatalf("round %d: the filter taken again read %q", round, got.Payload)
		}

		if err = again.Unsubscribe(ctx); err != nil {
			t.Fatalf("round %d: Unsubscribe: %v", round, err)
		}
	}
}

// TestTLS: under a TLS configuration that names no server, the broker's
// certificate is verified for its address's host; one that trusts nobody
// is refused.
func TestTLS(t *testing.T) {
	t.Parallel()

	// A certificate for the loopback address, and whom to trust for it.
	issued := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(issued.Close)

	trusting := &tls.Config{RootCAs: x509.NewCertPool(), MinVersion: tls.VersionTLS12}
	trusting.RootCAs.AddCert(issued.Certificate())

	// A server that does the handshake and hangs up: it speaks no MQTT,
	// and the handshake is what is seen.
	var config net.ListenConfig

	plain, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	server := tls.NewListener(plain, &tls.Config{Certificates: issued.TLS.Certificates, MinVersion: tls.VersionTLS12})

	t.Cleanup(func() { _ = server.Close() })

	go func() {
		for {
			conn, gone := server.Accept()
			if gone != nil {
				return
			}

			if secured, ok := conn.(*tls.Conn); ok {
				_ = secured.HandshakeContext(t.Context())
			}

			_ = conn.Close()
		}
	}()

	address := server.Addr().String()

	_, err = mqtt.Dial(soon(t), mqtt.Options{Address: address, TLS: trusting})
	if err == nil || strings.Contains(err.Error(), "ServerName") || strings.Contains(err.Error(), "certificate") {
		t.Errorf("Dial of a TLS server it trusts, that speaks no MQTT = %v; want the handshake done", err)
	}

	_, err = mqtt.Dial(soon(t), mqtt.Options{Address: address, TLS: &tls.Config{MinVersion: tls.VersionTLS12}})
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("Dial of a TLS server it does not trust = %v, want its certificate refused", err)
	}
}

// TestLost: when the connection goes, the Conn is done and says it is
// lost; a subscription hands over what it had, then the loss; nothing more
// is sent.
func TestLost(t *testing.T) {
	t.Parallel()

	broker := started(t)
	conn := dialled(t, broker, mqtt.Options{})
	ctx := soon(t)

	sub, err := conn.Subscribe(ctx, "t", mqtt.AtMostOnce)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if conn.Err() != nil {
		t.Fatalf("Err of a Conn that lives = %v", conn.Err())
	}

	broker.Publish(mqtt.Message{Topic: "t", Payload: []byte("last words")})

	// Read once it is in, so that the cut cannot overtake it.
	if got := read(ctx, t, sub); string(got.Payload) != "last words" {
		t.Fatalf("Next = %+v", got)
	}

	broker.Drop()

	select {
	case <-conn.Done():
	case <-ctx.Done():
		t.Fatal("the Conn is not done after the connection was cut")
	}

	if err = conn.Err(); !errors.Is(err, mqtt.ErrLost) || errors.Is(err, mqtt.ErrClosed) {
		t.Errorf("Err after the connection was cut = %v, want ErrLost", err)
	}

	if _, err = sub.Next(ctx); !errors.Is(err, mqtt.ErrLost) {
		t.Errorf("Next after the connection was cut = %v, want ErrLost", err)
	}

	if err = conn.Publish(ctx, mqtt.Message{Topic: "t"}); !errors.Is(err, mqtt.ErrLost) {
		t.Errorf("Publish after the connection was cut = %v, want ErrLost", err)
	}

	if _, err = conn.Subscribe(ctx, "u", mqtt.AtMostOnce); !errors.Is(err, mqtt.ErrLost) {
		t.Errorf("Subscribe after the connection was cut = %v, want ErrLost", err)
	}

	if err = sub.Unsubscribe(ctx); err != nil || conn.Close() != nil {
		t.Errorf("Unsubscribe and Close after the connection was cut = %v", err)
	}

	if err = conn.Err(); !errors.Is(err, mqtt.ErrLost) {
		t.Errorf("Err after Close of a Conn that was lost = %v, want ErrLost still", err)
	}
}

// TestClosed: a Conn closed by its owner is done, says so, and tells the
// broker, which forgets its subscriptions.
func TestClosed(t *testing.T) {
	t.Parallel()

	broker := started(t)
	conn := dialled(t, broker, mqtt.Options{})
	ctx := soon(t)

	sub, err := conn.Subscribe(ctx, "t", mqtt.AtMostOnce)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if err = conn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case <-conn.Done():
	default:
		t.Error("the Conn is not done after Close")
	}

	if err = conn.Close(); err != nil || !errors.Is(conn.Err(), mqtt.ErrClosed) {
		t.Errorf("Close again = %v, Err = %v", err, conn.Err())
	}

	if _, err = sub.Next(ctx); !errors.Is(err, mqtt.ErrClosed) {
		t.Errorf("Next after Close = %v, want ErrClosed", err)
	}

	if err = conn.Publish(ctx, mqtt.Message{Topic: "t"}); !errors.Is(err, mqtt.ErrClosed) {
		t.Errorf("Publish after Close = %v, want ErrClosed", err)
	}

	for len(broker.Filters()) != 0 {
		select {
		case <-ctx.Done():
			t.Fatalf("the broker still holds %v after Close", broker.Filters())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestContexts: a wait ends with its context: for a message that does not
// come, and for a broker that does not answer.
func TestContexts(t *testing.T) {
	t.Parallel()

	broker := started(t)
	conn := dialled(t, broker, mqtt.Options{})

	sub, err := conn.Subscribe(soon(t), "quiet", mqtt.AtMostOnce)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	brief, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	if _, err = sub.Next(brief); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Next with nothing to read = %v, want the deadline", err)
	}

	// A server that takes the connection and says nothing.
	var config net.ListenConfig

	silent, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	defer func() { _ = silent.Close() }()

	brief, cancel = context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	if _, err = mqtt.Dial(
		brief,
		mqtt.Options{Address: silent.Addr().String()},
	); !errors.Is(
		err,
		context.DeadlineExceeded,
	) {
		t.Errorf("Dial of a broker that does not answer = %v, want the deadline", err)
	}

	// Nothing listens there.
	gone := silent.Addr().String()
	_ = silent.Close()

	if _, err = mqtt.Dial(soon(t), mqtt.Options{Address: gone}); err == nil || errors.Is(err, mqtt.ErrArgument) {
		t.Errorf("Dial of an address nothing listens on = %v", err)
	}
}

// TestArguments: what cannot be sent is ErrArgument, and nothing is sent.
func TestArguments(t *testing.T) {
	t.Parallel()

	broker := started(t)

	for name, opts := range map[string]mqtt.Options{
		"no address":                     {},
		"an address with no port":        {Address: "localhost"},
		"a version that is none":         {Address: broker.Addr(), Version: 5},
		"an identifier too long for 3.1": {Address: broker.Addr(), Version: mqtt.V31, ClientID: strings.Repeat("x", 24)},
		"a keep-alive under a second":    {Address: broker.Addr(), KeepAlive: time.Millisecond},
	} {
		if _, err := mqtt.Dial(t.Context(), opts); !errors.Is(err, mqtt.ErrArgument) {
			t.Errorf("Dial with %s = %v, want ErrArgument", name, err)
		}
	}

	if hellos := broker.Hellos(); len(hellos) != 0 {
		t.Fatalf("the refused options opened %+v", hellos)
	}

	conn := dialled(t, broker, mqtt.Options{Version: mqtt.V311, ClientID: strings.Repeat("x", 24)})
	ctx := soon(t)

	for name, do := range map[string]func() error{
		"a topic with a wildcard": func() error { return conn.Publish(ctx, mqtt.Message{Topic: "a/+"}) },
		"no topic":                func() error { return conn.Publish(ctx, mqtt.Message{}) },
		"a quality of service of three": func() error {
			return conn.Publish(ctx, mqtt.Message{Topic: "a", QoS: 3})
		},
		"a filter that is none": func() error {
			_, err := conn.Subscribe(ctx, "a/b#", mqtt.AtMostOnce)

			return err
		},
		"a subscription at three": func() error {
			_, err := conn.Subscribe(ctx, "a", 3)

			return err
		},
	} {
		if err := do(); !errors.Is(err, mqtt.ErrArgument) {
			t.Errorf("%s = %v, want ErrArgument", name, err)
		}
	}

	if len(broker.Published())+len(broker.Filters()) != 0 {
		t.Errorf("the refused arguments sent %v and %v", broker.Published(), broker.Filters())
	}
}

// TestTopics: the standard's own examples of filters, and of what is none.
func TestTopics(t *testing.T) {
	t.Parallel()

	for _, each := range []struct {
		filter, topic string
		want          bool
	}{
		{"sport/tennis/player1/#", "sport/tennis/player1", true},
		{"sport/tennis/player1/#", "sport/tennis/player1/ranking", true},
		{"sport/tennis/player1/#", "sport/tennis/player1/score/wimbledon", true},
		{"sport/#", "sport", true},
		{"#", "sport/tennis", true},
		{"sport/tennis/+", "sport/tennis/player1", true},
		{"sport/tennis/+", "sport/tennis/player1/ranking", false},
		{"sport/+", "sport", false},
		{"sport/+", "sport/", true},
		{"+/+", "/finance", true},
		{"/+", "/finance", true},
		{"+", "/finance", false},
		{"+/tennis/#", "sport/tennis", true},
		{"sport/+/player1", "sport/tennis/player1", true},
		{"sport/tennis", "sport/tennis", true},
		{"sport/tennis", "sport/Tennis", false},
		{"sport/tennis", "sport/tennis/", false},
		{"#", "$SYS/broker", false},
		{"+/monitor/Clients", "$SYS/monitor/Clients", false},
		{"$SYS/#", "$SYS/broker", true},
		{"$SYS/monitor/+", "$SYS/monitor/Clients", true},
		{"sport/tennis#", "sport/tennis#", false},
		{"sport/tennis", "sport/+", false},
	} {
		if got := mqtt.Match(each.filter, each.topic); got != each.want {
			t.Errorf("Match(%q, %q) = %t", each.filter, each.topic, got)
		}
	}

	for filter, valid := range map[string]bool{
		"#": true, "+": true, "sport/tennis/#": true, "+/tennis/#": true, "sport/+/player1": true, "/": true,
		"sport+": false, "sport/tennis#": false, "sport/tennis/#/ranking": false, "#/a": false, "a/+b": false,
		"": false, "a\x00b": false, "a\xffb": false, strings.Repeat("a", 65536): false,
	} {
		if err := mqtt.CheckFilter(filter); (err == nil) != valid || err != nil && !errors.Is(err, mqtt.ErrArgument) {
			t.Errorf("CheckFilter(%.20q) = %v, want valid %t", filter, err, valid)
		}
	}

	for topic, valid := range map[string]bool{
		"a": true, "/": true, "a/b c/d": true, strings.Repeat("a", 65535): true,
		"a/+": false, "#": false, "": false, "a\x00": false, strings.Repeat("a", 65536): false,
	} {
		if err := mqtt.CheckTopic(topic); (err == nil) != valid {
			t.Errorf("CheckTopic(%.20q) = %v, want valid %t", topic, err, valid)
		}
	}
}

// TestThePasswordStaysIn: an Options formatted or logged shows what it
// is, and not its password.
func TestThePasswordStaysIn(t *testing.T) {
	t.Parallel()

	opts := mqtt.Options{Address: "broker.test:1883", Username: "someone", Password: "hunter2"}

	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
		printed := fmt.Sprintf(verb, opts) + fmt.Sprintf(verb, &opts)
		if strings.Contains(printed, "hunter2") || strings.Contains(printed, fmt.Sprintf("%x", "hunter2")) ||
			!strings.Contains(printed, "broker.test:1883") {
			t.Errorf("Options under %s prints %s", verb, printed)
		}
	}

	if logged := opts.LogValue().String(); strings.Contains(logged, "hunter2") || !strings.Contains(logged, "someone") {
		t.Errorf("Options in a log record = %s", logged)
	}

	type holder struct {
		Options mqtt.Options `json:"options"`
	}

	marshalled, err := json.Marshal(holder{opts})
	if err != nil || strings.Contains(string(marshalled), "hunter2") ||
		!strings.Contains(string(marshalled), "someone") {
		t.Errorf("a struct that holds Options, as JSON = %s, %v", marshalled, err)
	}
}
