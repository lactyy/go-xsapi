package rta

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/df-mc/go-xsapi/internal"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
)

// Conn represents a connection between the real-time activity services. It can
// be established from Dialer with an authorization token that relies on the
// party 'https://xboxlive.com/'.
//
// A Conn controls subscriptions real-timely under a websocket connection. An
// index-specific JSON array is used for the communication. Conn is safe for
// concurrent use in multiple goroutines.
//
// SubscriptionHandlers are useful to handle any events that may occur in the subscriptions
// controlled by Conn, and can be stored atomically to a Subscription from [Subscription.Handle].
type Conn struct {
	conn *websocket.Conn

	sequences  [operationCapacity]atomic.Uint32
	expected   [operationCapacity]map[uint32]chan<- *handshake
	expectedMu sync.RWMutex

	subscriptions   map[uint32]*Subscription
	subscriptionsMu sync.RWMutex

	log *slog.Logger

	once   sync.Once
	closed chan struct{}
}

// Subscribe attempts to subscribe with the specific resource URI, with the [context.Context]
// to be used during the handshake. A Subscription may be returned, which contains an ID
// and Custom data as the result of handshake.
func (c *Conn) Subscribe(ctx context.Context, resourceURI string) (*Subscription, error) {
	sequence := c.sequences[operationSubscribe].Add(1)
	hand, err := c.shake(operationSubscribe, sequence, []any{resourceURI})
	if err != nil {
		return nil, err
	}
	defer c.release(operationSubscribe, sequence)
	select {
	case h := <-hand:
		switch h.status {
		case StatusOK:
			if len(h.payload) < 2 {
				return nil, &OutOfRangeError{
					Payload: h.payload,
					Index:   1,
				}
			}
			sub := &Subscription{}
			if err := json.Unmarshal(h.payload[0], &sub.ID); err != nil {
				return nil, fmt.Errorf("decode subscription ConnectionID: %w", err)
			}
			sub.Custom = h.payload[1]

			c.subscriptionsMu.Lock()
			c.subscriptions[sub.ID] = sub
			c.subscriptionsMu.Unlock()
			return sub, nil
		default:
			return nil, unexpectedStatusCode(h.status, h.payload)
		}
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.closed:
		return nil, net.ErrClosed
	}
}

// Unsubscribe attempts to unsubscribe with a Subscription associated with an ID, with
// the [context.Context] to be used during the handshake. An error may be returned.
func (c *Conn) Unsubscribe(ctx context.Context, sub *Subscription) error {
	sequence := c.sequences[operationUnsubscribe].Add(1)
	hand, err := c.shake(operationUnsubscribe, sequence, []any{sub.ID})
	if err != nil {
		return err
	}
	defer c.release(operationUnsubscribe, sequence)
	select {
	case h := <-hand:
		if h.status != StatusOK {
			return unexpectedStatusCode(h.status, h.payload)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return net.ErrClosed
	}
}

// Subscription represents a subscription contracted with the resource URI available through
// the real-time activity service. A Subscription may be contracted via Conn.Subscribe.
type Subscription struct {
	ID     uint32
	Custom json.RawMessage

	h  SubscriptionHandler
	mu sync.Mutex
}

func (s *Subscription) Handle(h SubscriptionHandler) {
	s.mu.Lock()
	s.h = h
	s.mu.Unlock()
}

func (s *Subscription) handler() SubscriptionHandler {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.h == nil {
		return NopSubscriptionHandler{}
	}
	return s.h
}

type SubscriptionHandler interface {
	HandleEvent(custom json.RawMessage)
}

type NopSubscriptionHandler struct{}

func (NopSubscriptionHandler) HandleEvent(json.RawMessage) {}

// write attempts to write a JSON array with header and the body. A background context is
// used as no context perceived by the parent goroutine should be used to a websocket method
// to avoid closing the connection if it has cancelled or exceeded a deadline.
func (c *Conn) write(typ uint32, payload []any) error {
	return wsjson.Write(context.Background(), c.conn, append([]any{typ}, payload...))
}

// read goes as a background goroutine of Conn, reading a JSON array from the websocket
// connection and decoding a header needed to indicate which message should be handled.
func (c *Conn) read() {
	for {
		var payload []json.RawMessage
		if err := wsjson.Read(context.Background(), c.conn, &payload); err != nil {
			_ = c.Close()
			return
		}
		typ, err := readHeader(payload)
		if err != nil {
			c.log.Error("error reading header", internal.ErrAttr(err))
			continue
		}
		go c.handleMessage(typ, payload[1:])
	}
}

// Close closes the websocket connection with websocket.StatusNormalClosure.
func (c *Conn) Close() (err error) {
	c.once.Do(func() {
		close(c.closed)
		err = c.conn.Close(websocket.StatusNormalClosure, "")
	})
	return err
}

// handleMessage handles a message received in read with the type.
func (c *Conn) handleMessage(typ uint32, payload []json.RawMessage) {
	switch typ {
	case typeSubscribe, typeUnsubscribe: // Subscribe & Unsubscribe handshake response
		h, err := readHandshake(payload)
		if err != nil {
			c.log.Error("error reading handshake response", internal.ErrAttr(err))
			return
		}
		op := typeToOperation(typ)
		c.expectedMu.RLock()
		defer c.expectedMu.RUnlock()
		hand, ok := c.expected[op][h.sequence]
		if !ok {
			c.log.Debug("unexpected handshake response", slog.Group("message", "type", typ, "sequence", h.sequence))
			return
		}
		hand <- h
	case typeEvent:
		if len(payload) < 2 {
			c.log.Debug("event message has no custom")
			return
		}
		var subscriptionID uint32
		if err := json.Unmarshal(payload[0], &subscriptionID); err != nil {
			c.log.Error("error decoding subscription ID", internal.ErrAttr(err))
		}
		c.subscriptionsMu.Lock()
		defer c.subscriptionsMu.Unlock()
		sub, ok := c.subscriptions[subscriptionID]
		if ok {
			go sub.handler().HandleEvent(payload[1])
		}
		c.log.Debug("received event", slog.Group("message", "type", typ, "custom", payload[0]))
	default:
		c.log.Debug("received an unexpected message", slog.Group("message", "type", typ))
	}
}

// An OutOfRangeError occurs when reading values from payload received from the service.
// The Payload specifies the remaining values included in the payload, and the Index specifies
// a length of values that is missing from the payload.
type OutOfRangeError struct {
	Payload []json.RawMessage
	Index   int
}

func (e *OutOfRangeError) Error() string {
	return fmt.Sprintf("xsapi/rta: index out of range [%d] with length %d", e.Index, len(e.Payload))
}

// readHeader decodes a header from the first 1 value from the payload. An OutOfRangeError
// may be returned if the payload has not enough length to read.
func readHeader(payload []json.RawMessage) (typ uint32, err error) {
	if len(payload) < 1 {
		return typ, &OutOfRangeError{
			Payload: payload,
			Index:   0,
		}
	}
	return typ, json.Unmarshal(payload[0], &typ)
}

// readHandshake decodes a handshake from the first 2 values from the payload.
// An OutOfRangeError may be returned if the payload has not enough length to read.
func readHandshake(payload []json.RawMessage) (*handshake, error) {
	if len(payload) < 2 {
		return nil, &OutOfRangeError{
			Payload: payload,
			Index:   2,
		}
	}
	h := &handshake{}
	if err := json.Unmarshal(payload[0], &h.sequence); err != nil {
		return nil, fmt.Errorf("decode sequence: %w", err)
	}
	if err := json.Unmarshal(payload[1], &h.status); err != nil {
		return nil, fmt.Errorf("decode status code: %w", err)
	}
	h.payload = payload[2:]
	return h, nil
}

// unexpectedStatusCode wraps an UnexpectedStatusError from the status.
// If the payload has more than one remaining values, it will try to decode
// them as an error message.
func unexpectedStatusCode(status int32, payload []json.RawMessage) error {
	err := &UnexpectedStatusError{Code: status}
	if len(payload) >= 1 {
		_ = json.Unmarshal(payload[0], &err.Message)
	}
	return err
}
