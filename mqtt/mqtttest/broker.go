// Package mqtttest is an MQTT broker for tests: it listens on the loopback
// interface, takes sessions of MQTT 3.1 and 3.1.1, routes what is
// published to who subscribed, keeps retained messages, and answers at
// the three qualities of service. A test decides who gets in, sees what
// every client sent, publishes of its own, and cuts the connections.
//
// It is no broker to rely on: it keeps no session from one connection to
// the next, sends nothing twice, and has no limits.
package mqtttest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/eclipse/paho.mqtt.golang/packets"

	"github.com/duncanite/go-dyson/mqtt"
)

// Code is how a broker answers a session: the protocol's return codes.
type Code byte

// The answers.
const (
	Accepted Code = iota
	BadVersion
	BadClientID
	Unavailable
	BadCredentials
	NotAuthorized
)

// Hello is a session as a client opened it.
type Hello struct {
	// Protocol and Version are the protocol's name and level: MQIsdp and 3
	// for MQTT 3.1, MQTT and 4 for 3.1.1.
	Protocol string
	Version  byte
	ClientID string
	Username string
	Password string
	// KeepAlive is in seconds.
	KeepAlive uint16
	Clean     bool
}

// Broker is the fake. A test sets Accept, Handle and Refused before a
// client connects.
type Broker struct {
	// Accept answers each session; nil accepts them all.
	Accept func(Hello) Code
	// Handle is called with each message a client publishes, once it is
	// routed, on that client's goroutine; it may call Publish.
	Handle func(mqtt.Message)
	// Refused is the filters whose subscription is refused.
	Refused map[string]bool

	listener net.Listener
	serving  sync.WaitGroup

	// stopped is closed when the broker is.
	stopped chan struct{}

	mu        sync.Mutex
	held      chan struct{}
	sessions  map[*session]struct{}
	retained  map[string]mqtt.Message
	hellos    []Hello
	published []mqtt.Message
	filters   []string
	next      uint16
}

// session is one client's connection: what it subscribed to, and at
// which quality of service.
type session struct {
	conn net.Conn

	write   sync.Mutex
	granted map[string]mqtt.QoS
}

// New starts a broker on a port of the loopback interface. The caller
// closes it.
func New() *Broker {
	var config net.ListenConfig

	// Bound at once: there is nothing for a context to cancel.
	listener, err := config.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}

	broker := &Broker{
		listener: listener,
		stopped:  make(chan struct{}),
		sessions: map[*session]struct{}{},
		retained: map[string]mqtt.Message{},
	}

	broker.serving.Go(broker.accept)

	return broker
}

// Addr is the broker's address, host and port.
func (b *Broker) Addr() string { return b.listener.Addr().String() }

// Close stops the broker and cuts every connection.
func (b *Broker) Close() {
	_ = b.listener.Close()

	close(b.stopped)

	b.Drop()
	b.serving.Wait()
}

// Drop cuts every connection, with no word to the clients.
func (b *Broker) Drop() {
	b.mu.Lock()
	defer b.mu.Unlock()

	for one := range b.sessions {
		_ = one.conn.Close()
	}
}

// Hold has the broker keep back its answer to every subscription asked
// for from now on, each until the function it returns is called; a
// session that waits so reads nothing more meanwhile.
func (b *Broker) Hold() (grant func()) {
	held := make(chan struct{})

	b.mu.Lock()
	b.held = held
	b.mu.Unlock()

	return func() {
		select {
		case held <- struct{}{}:
		case <-b.stopped:
		}
	}
}

// Hellos is the sessions clients opened, accepted or not, in order.
func (b *Broker) Hellos() []Hello {
	b.mu.Lock()
	defer b.mu.Unlock()

	return append([]Hello(nil), b.hellos...)
}

// Published is the messages clients published, in order.
func (b *Broker) Published() []mqtt.Message {
	b.mu.Lock()
	defer b.mu.Unlock()

	return append([]mqtt.Message(nil), b.published...)
}

// Filters is the filters clients hold a subscription to now, in the
// order they subscribed.
func (b *Broker) Filters() []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return append([]string(nil), b.filters...)
}

// Publish sends a message to every client subscribed to its topic, at the
// lower of its quality of service and the subscription's, and keeps it
// when it is to be retained.
func (b *Broker) Publish(message mqtt.Message) {
	b.mu.Lock()

	if message.Retain {
		b.retained[message.Topic] = message
	}

	type delivery struct {
		to  *session
		qos mqtt.QoS
		id  uint16
	}

	var deliveries []delivery

	for one := range b.sessions {
		for filter, granted := range one.granted {
			if mqtt.Match(filter, message.Topic) {
				b.next++
				deliveries = append(deliveries, delivery{to: one, qos: min(granted, message.QoS), id: b.next})

				break
			}
		}
	}

	b.mu.Unlock()

	// A message routed to a live subscription is not marked retained.
	for _, each := range deliveries {
		each.to.publish(message, each.qos, each.id, false)
	}
}

// Push sends a message to every client, subscribed to its topic or not,
// at quality of service 0: what reaches a client from a broker that has
// not yet read its unsubscription.
func (b *Broker) Push(message mqtt.Message) {
	b.mu.Lock()

	all := make([]*session, 0, len(b.sessions))
	for one := range b.sessions {
		all = append(all, one)
	}

	b.mu.Unlock()

	for _, one := range all {
		one.publish(message, mqtt.AtMostOnce, 0, false)
	}
}

func (b *Broker) accept() {
	for {
		conn, err := b.listener.Accept()
		if err != nil {
			return
		}

		b.serving.Go(func() { b.serve(conn) })
	}
}

// serve is one connection: its session, then its packets until it ends.
func (b *Broker) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	first, err := packets.ReadPacket(conn)
	if err != nil {
		return
	}

	hello, ok := first.(*packets.ConnectPacket)
	if !ok {
		return
	}

	opened := Hello{
		Protocol: hello.ProtocolName, Version: hello.ProtocolVersion, ClientID: hello.ClientIdentifier,
		Username: hello.Username, Password: string(hello.Password), KeepAlive: hello.Keepalive,
		Clean: hello.CleanSession,
	}

	answer := Accepted
	if b.Accept != nil {
		answer = b.Accept(opened)
	}

	one := &session{conn: conn, granted: map[string]mqtt.QoS{}}

	b.mu.Lock()
	b.hellos = append(b.hellos, opened)

	if answer == Accepted {
		b.sessions[one] = struct{}{}
	}

	b.mu.Unlock()

	ack, ok := packets.NewControlPacket(packets.Connack).(*packets.ConnackPacket)
	if !ok {
		return
	}

	ack.ReturnCode = byte(answer)

	if one.send(ack) != nil || answer != Accepted {
		return
	}

	defer b.forget(one)

	for {
		packet, err := packets.ReadPacket(conn)
		if err != nil || !b.take(one, packet) {
			return
		}
	}
}

// forget ends a session: its subscriptions go with it.
func (b *Broker) forget(one *session) {
	b.mu.Lock()
	defer b.mu.Unlock()

	delete(b.sessions, one)

	for filter := range one.granted {
		b.unlist(filter)
	}
}

// unlist takes one subscription to a filter off the list; the lock is
// held.
func (b *Broker) unlist(filter string) {
	for index, held := range b.filters {
		if held == filter {
			b.filters = append(b.filters[:index], b.filters[index+1:]...)

			return
		}
	}
}

// take answers one packet of a session, and says whether the session
// goes on.
func (b *Broker) take(one *session, packet packets.ControlPacket) bool {
	switch asked := packet.(type) {
	case *packets.PublishPacket:
		return b.publishedBy(one, asked)
	case *packets.SubscribePacket:
		return b.subscribe(one, asked)
	case *packets.UnsubscribePacket:
		b.mu.Lock()

		for _, filter := range asked.Topics {
			if _, held := one.granted[filter]; held {
				delete(one.granted, filter)
				b.unlist(filter)
			}
		}

		b.mu.Unlock()

		return one.answer(packets.Unsuback, asked.MessageID)
	case *packets.PubrelPacket:
		return one.answer(packets.Pubcomp, asked.MessageID)
	case *packets.PubrecPacket:
		return one.answer(packets.Pubrel, asked.MessageID)
	case *packets.PingreqPacket:
		return one.send(packets.NewControlPacket(packets.Pingresp)) == nil
	case *packets.DisconnectPacket:
		return false
	default:
		// The acknowledgements of what the broker sent need no answer.
		return true
	}
}

// publishedBy takes a message a client published: routes it, hands it to
// the test, and only then acknowledges it at its quality of service, so
// that a client whose Publish has returned finds the test's handler done.
func (b *Broker) publishedBy(one *session, sent *packets.PublishPacket) bool {
	message := mqtt.Message{
		Topic: sent.TopicName, Payload: append([]byte(nil), sent.Payload...), QoS: mqtt.QoS(sent.Qos),
		Retain: sent.Retain,
	}

	b.mu.Lock()
	b.published = append(b.published, message)
	b.mu.Unlock()

	b.Publish(message)

	if b.Handle != nil {
		b.Handle(message)
	}

	switch message.QoS {
	case mqtt.AtLeastOnce:
		return one.answer(packets.Puback, sent.MessageID)
	case mqtt.ExactlyOnce:
		return one.answer(packets.Pubrec, sent.MessageID)
	case mqtt.AtMostOnce:
	}

	return true
}

// granting waits for the test's word when it holds the grants back, and
// says whether the broker still stands.
func (b *Broker) granting() bool {
	b.mu.Lock()
	held := b.held
	b.mu.Unlock()

	if held == nil {
		return true
	}

	select {
	case <-held:
		return true
	case <-b.stopped:
		return false
	}
}

// subscribe grants a session's filters, or refuses those the test said
// to, and sends what is retained on the topics they match.
func (b *Broker) subscribe(one *session, asked *packets.SubscribePacket) bool {
	const refused = 0x80

	if !b.granting() {
		return false
	}

	ack, ok := packets.NewControlPacket(packets.Suback).(*packets.SubackPacket)
	if !ok {
		return false
	}

	ack.MessageID = asked.MessageID

	type kept struct {
		message mqtt.Message
		qos     mqtt.QoS
		id      uint16
	}

	var retained []kept

	b.mu.Lock()

	for index, filter := range asked.Topics {
		if b.Refused[filter] || mqtt.CheckFilter(filter) != nil {
			ack.ReturnCodes = append(ack.ReturnCodes, refused)

			continue
		}

		granted := mqtt.QoS(asked.Qoss[index])
		ack.ReturnCodes = append(ack.ReturnCodes, byte(granted))

		if _, held := one.granted[filter]; !held {
			b.filters = append(b.filters, filter)
		}

		one.granted[filter] = granted

		for topic, message := range b.retained {
			if mqtt.Match(filter, topic) {
				b.next++
				retained = append(retained, kept{message: message, qos: min(granted, message.QoS), id: b.next})
			}
		}
	}

	b.mu.Unlock()

	if one.send(ack) != nil {
		return false
	}

	for _, each := range retained {
		one.publish(each.message, each.qos, each.id, true)
	}

	return true
}

// errNoPacket is a packet the carrier's library could not make.
var errNoPacket = errors.New("no such packet")

func (s *session) send(packet packets.ControlPacket) error {
	if packet == nil {
		return errNoPacket
	}

	s.write.Lock()
	defer s.write.Unlock()

	if err := packet.Write(s.conn); err != nil {
		return fmt.Errorf("writing a packet: %w", err)
	}

	return nil
}

// answer sends the acknowledgement of one kind for a packet's id.
func (s *session) answer(kind byte, identifier uint16) bool {
	var packet packets.ControlPacket

	switch ack := packets.NewControlPacket(kind).(type) {
	case *packets.PubackPacket:
		ack.MessageID, packet = identifier, ack
	case *packets.PubrecPacket:
		ack.MessageID, packet = identifier, ack
	case *packets.PubrelPacket:
		ack.MessageID, packet = identifier, ack
	case *packets.PubcompPacket:
		ack.MessageID, packet = identifier, ack
	case *packets.UnsubackPacket:
		ack.MessageID, packet = identifier, ack
	}

	return s.send(packet) == nil
}

// publish sends a message to the session's client.
func (s *session) publish(message mqtt.Message, qos mqtt.QoS, identifier uint16, retained bool) {
	packet, ok := packets.NewControlPacket(packets.Publish).(*packets.PublishPacket)
	if !ok {
		return
	}

	packet.TopicName, packet.Payload = message.Topic, message.Payload
	packet.Qos, packet.Retain = byte(qos), retained

	if qos > mqtt.AtMostOnce {
		packet.MessageID = identifier
	}

	_ = s.send(packet)
}
