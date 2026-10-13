// Package publisher sends MQTT messages. MQTTPublisher is the real client,
// Fake records messages for tests.
package publisher

import "fmt"

// Message is a single MQTT publish request. QoS is chosen per message: the
// state is telemetry (QoS 0, superseded every round), the outage topics are
// events (QoS 1, queued by the Pi's bridge while its uplink is down).
type Message struct {
	Topic    string
	Payload  string
	Retained bool
	QOS      byte
}

// Publisher is what the main loop needs.
type Publisher interface {
	Publish(Message) error
	Close() error
}

// Options are the connection settings.
type Options struct {
	Broker    string
	ClientID  string
	Username  string
	Password  string
	TLSCACert string
	WillQOS   byte
}

// Fake records every published Message; it models a broker's retained store
// when shared across several process lifetimes.
type Fake struct {
	Messages []Message
	// Fail, when set, makes every publish fail.
	Fail   bool
	Failed []string
	Closed bool
}

// Publish implements Publisher.
func (f *Fake) Publish(m Message) error {
	if f.Fail {
		f.Failed = append(f.Failed, m.Topic)
		return fmt.Errorf("fake publish failure for %q", m.Topic)
	}
	f.Messages = append(f.Messages, m)
	return nil
}

// Close implements Publisher.
func (f *Fake) Close() error { f.Closed = true; return nil }

// Retained returns what a broker would deliver to a new subscriber of topic.
// An empty retained payload clears the topic.
func (f *Fake) Retained(topic string) (Message, bool) {
	var last Message
	found := false
	for _, m := range f.Messages {
		if m.Topic != topic || !m.Retained {
			continue
		}
		last, found = m, m.Payload != ""
	}
	return last, found
}

// All returns the messages published to topic, in order.
func (f *Fake) All(topic string) []Message {
	var out []Message
	for _, m := range f.Messages {
		if m.Topic == topic {
			out = append(out, m)
		}
	}
	return out
}
