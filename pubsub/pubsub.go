// Copyright 2018 The Go Cloud Development Kit Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package pubsub provides an easy and portable way to interact with publish/
// subscribe systems.
//
// Subpackages contain distinct implementations of pubsub for various providers,
// including Cloud and on-prem solutions. For example, "gcspubsub" supports
// Google Cloud Pub/Sub. Your application should import one of these
// provider-specific subpackages and use its exported functions to get a
// *Topic and/or *Subscription; do not use the NewTopic/NewSubscription
// functions in this package. For example:
//
//  topic := mempubsub.NewTopic()
//  err := topic.Send(ctx.Background(), &pubsub.Message{Body: []byte("hi"))
//  ...
//
// Then, write your application code using the *Topic/*Subscription types. You
// can easily reconfigure your initialization code to choose a different provider.
// You can develop your application locally using memblob, or deploy it to
// multiple Cloud providers. You may find http://github.com/google/wire useful
// for managing your initialization code.
package pubsub // import "gocloud.dev/pubsub"

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	gax "github.com/googleapis/gax-go"
	"gocloud.dev/gcerrors"
	"gocloud.dev/internal/batcher"
	"gocloud.dev/internal/gcerr"
	"gocloud.dev/internal/retry"
	"gocloud.dev/internal/trace"
	"gocloud.dev/pubsub/driver"
)

// Message contains data to be published.
type Message struct {
	// Body contains the content of the message.
	Body []byte

	// Metadata has key/value metadata for the message.
	Metadata map[string]string

	// ack is a closure that queues this message for acknowledgement.
	ack func()

	// mu guards isAcked in case Ack() is called concurrently.
	mu sync.Mutex

	// isAcked tells whether this message has already had its Ack method
	// called.
	isAcked bool
}

// Ack acknowledges the message, telling the server that it does not need to be
// sent again to the associated Subscription. It returns immediately, but the
// actual ack is sent in the background, and is not guaranteed to succeed.
func (m *Message) Ack() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.isAcked {
		panic(fmt.Sprintf("Ack() called twice on message: %+v", m))
	}
	m.ack()
	m.isAcked = true
}

// Topic publishes messages to all its subscribers.
type Topic struct {
	driver  driver.Topic
	batcher driver.Batcher
	mu      sync.Mutex
	err     error

	// cancel cancels all SendBatch calls.
	cancel func()
}

type msgErrChan struct {
	msg     *Message
	errChan chan error
}

// Send publishes a message. It only returns after the message has been
// sent, or failed to be sent. Send can be called from multiple goroutines
// at once.
func (t *Topic) Send(ctx context.Context, m *Message) (err error) {
	ctx = trace.StartSpan(ctx, "gocloud.dev/pubsub.Topic.Send")
	defer func() { trace.EndSpan(ctx, err) }()

	// Check for doneness before we do any work.
	if err := ctx.Err(); err != nil {
		return err // Return context errors unwrapped.
	}
	t.mu.Lock()
	err = t.err
	t.mu.Unlock()
	if err != nil {
		return err // t.err wrapped when set
	}
	return t.batcher.Add(ctx, m)
}

// Shutdown flushes pending message sends and disconnects the Topic.
// It only returns after all pending messages have been sent.
func (t *Topic) Shutdown(ctx context.Context) (err error) {
	ctx = trace.StartSpan(ctx, "gocloud.dev/pubsub.Topic.Shutdown")
	defer func() { trace.EndSpan(ctx, err) }()

	t.mu.Lock()
	t.err = gcerr.Newf(gcerr.FailedPrecondition, nil, "pubsub: Topic closed")
	t.mu.Unlock()
	c := make(chan struct{})
	go func() {
		defer close(c)
		t.batcher.Shutdown()
	}()
	select {
	case <-ctx.Done():
	case <-c:
	}
	t.cancel()
	return ctx.Err()
}

// As converts i to provider-specific types.
//
// This function (and the other As functions in this package) are inherently
// provider-specific, and using them will make that part of your application
// non-portable, so use with care.
//
// See the documentation for the subpackage used to instantiate Bucket to see
// which type(s) are supported.
//
// Usage:
//
// 1. Declare a variable of the provider-specific type you want to access.
//
// 2. Pass a pointer to it to As.
//
// 3. If the type is supported, As will return true and copy the
// provider-specific type into your variable. Otherwise, it will return false.
//
// Provider-specific types that are intended to be mutable will be exposed
// as a pointer to the underlying type.
//
// See
// https://github.com/google/go-cloud/blob/master/internal/docs/design.md#as
// for more background.
func (t *Topic) As(i interface{}) bool {
	return t.driver.As(i)
}

// NewTopic is for use by provider implementations.
var NewTopic = newTopic

// newTopic makes a pubsub.Topic from a driver.Topic.
func newTopic(d driver.Topic) *Topic {
	callCtx, cancel := context.WithCancel(context.Background())
	handler := func(item interface{}) error {
		ms := item.([]*Message)
		var dms []*driver.Message
		for _, m := range ms {
			dm := &driver.Message{
				Body:     m.Body,
				Metadata: m.Metadata,
			}
			dms = append(dms, dm)
		}

		err := retry.Call(callCtx, gax.Backoff{}, d.IsRetryable, func() (err error) {
			ctx := trace.StartSpan(callCtx, "gocloud.dev/pubsub/driver.Topic.SendBatch")
			defer func() { trace.EndSpan(ctx, err) }()
			return d.SendBatch(ctx, dms)
		})
		if err != nil {
			return wrapError(d, err)
		}
		return nil
	}
	maxHandlers := 1
	b := batcher.New(reflect.TypeOf(&Message{}), maxHandlers, handler)
	t := &Topic{
		driver:  d,
		batcher: b,
		cancel:  cancel,
	}
	return t
}

// Subscription receives published messages.
type Subscription struct {
	driver driver.Subscription

	// ackBatcher makes batches of acks and sends them to the server.
	ackBatcher driver.Batcher
	cancel     func() // for canceling all SendAcks calls

	mu    sync.Mutex    // protects everything below
	q     []*Message    // local queue of messages downloaded from server
	err   error         // permanent error
	waitc chan struct{} // for goroutines waiting on ReceiveBatch
}

// Receive receives and returns the next message from the Subscription's queue,
// blocking and polling if none are available. This method can be called
// concurrently from multiple goroutines. The Ack() method of the returned
// Message has to be called once the message has been processed, to prevent it
// from being received again.
func (s *Subscription) Receive(ctx context.Context) (_ *Message, err error) {
	ctx = trace.StartSpan(ctx, "gocloud.dev/pubsub.Subscription.Receive")
	defer func() { trace.EndSpan(ctx, err) }()

	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		// The lock is always held here, at the top of the loop.
		if s.err != nil {
			// The Subscription is in a permanent error state. Return the error.
			return nil, s.err // s.err wrapped when set
		}
		if len(s.q) > 0 {
			// At least one message is available. Return it.
			m := s.q[0]
			s.q = s.q[1:]
			return m, nil
		}
		if s.waitc != nil {
			// A call to ReceiveBatch is in flight. Wait for it.
			waitc := s.waitc
			s.mu.Unlock()
			select {
			case <-waitc:
				s.mu.Lock()
				continue
			case <-ctx.Done():
				s.mu.Lock()
				return nil, ctx.Err()
			}
		}
		// No messages are available and there are no calls to ReceiveBatch in flight.
		// Make a call.
		s.waitc = make(chan struct{})
		s.mu.Unlock()
		// Even though the mutex is unlocked, only one goroutine can be here.
		// The only way here is if s.waitc was nil. This goroutine just set
		// s.waitc to non-nil while holding the lock.
		msgs, err := s.getNextBatch(ctx)
		s.mu.Lock()
		close(s.waitc)
		s.waitc = nil
		if err != nil {
			// This goroutine's call failed, perhaps because its context was done.
			// Some waiting goroutine will wake up when s.waitc is closed,
			// go to the top of the loop, and (since s.q is empty and s.waitc is
			// now nil) will try the RPC for itself.
			return nil, err
		}
		s.q = append(s.q, msgs...)
	}
}

// getNextBatch gets the next batch of messages from the server and returns it.
func (s *Subscription) getNextBatch(ctx context.Context) ([]*Message, error) {
	var msgs []*driver.Message
	for len(msgs) == 0 {
		err := retry.Call(ctx, gax.Backoff{}, s.driver.IsRetryable, func() error {
			var err error
			// TODO(#691): dynamically adjust maxMessages
			const maxMessages = 10
			msgs, err = s.driver.ReceiveBatch(ctx, maxMessages)
			return err
		})
		if err != nil {
			return nil, wrapError(s.driver, err)
		}
	}
	var q []*Message
	for _, m := range msgs {
		id := m.AckID
		q = append(q, &Message{
			Body:     m.Body,
			Metadata: m.Metadata,
			ack: func() {
				// Ignore the error channel. Errors are dealt with
				// in the ackBatcher handler.
				_ = s.ackBatcher.AddNoWait(id)
			},
		})
	}
	return q, nil
}

// Shutdown flushes pending ack sends and disconnects the Subscription.
func (s *Subscription) Shutdown(ctx context.Context) (err error) {
	ctx = trace.StartSpan(ctx, "gocloud.dev/pubsub.Subscription.Shutdown")
	defer func() { trace.EndSpan(ctx, err) }()

	s.mu.Lock()
	s.err = gcerr.Newf(gcerr.FailedPrecondition, nil, "pubsub: Subscription closed")
	s.mu.Unlock()
	c := make(chan struct{})
	go func() {
		defer close(c)
		s.ackBatcher.Shutdown()
	}()
	select {
	case <-ctx.Done():
	case <-c:
	}
	s.cancel()
	return ctx.Err()
}

// As converts i to provider-specific types.
// See Topic.As for more details.
func (s *Subscription) As(i interface{}) bool {
	return s.driver.As(i)
}

// NewSubscription is for use by provider implementations.
var NewSubscription = newSubscription

// newSubscription creates a Subscription from a driver.Subscription
// and a function to make a batcher that sends batches of acks to the provider.
// If newAckBatcher is nil, a default batcher implementation will be used.
func newSubscription(d driver.Subscription, newAckBatcher func(context.Context, *Subscription) driver.Batcher) *Subscription {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Subscription{
		driver: d,
		cancel: cancel,
	}
	if newAckBatcher == nil {
		newAckBatcher = defaultAckBatcher
	}
	s.ackBatcher = newAckBatcher(ctx, s)
	return s
}

// defaultAckBatcher creates a batcher for acknowledgements, for use with
// NewSubscription.
func defaultAckBatcher(ctx context.Context, s *Subscription) driver.Batcher {
	const maxHandlers = 1
	handler := func(items interface{}) error {
		ids := items.([]driver.AckID)
		err := retry.Call(ctx, gax.Backoff{}, s.driver.IsRetryable, func() (err error) {
			ctx = trace.StartSpan(ctx, "gocloud.dev/pubsub/driver.Subscription.SendAcks")
			defer func() { trace.EndSpan(ctx, err) }()
			return s.driver.SendAcks(ctx, ids)
		})
		// Remember a non-retryable error from SendAcks. It will be returned on the
		// next call to Receive.
		if err != nil {
			err = wrapError(s.driver, err)
			s.mu.Lock()
			s.err = err
			s.mu.Unlock()
		}
		return err
	}
	return batcher.New(reflect.TypeOf([]driver.AckID{}).Elem(), maxHandlers, handler)
}

type errorCoder interface {
	ErrorCode(error) gcerrors.ErrorCode
}

func wrapError(ec errorCoder, err error) error {
	// Don't wrap context errors.
	if _, ok := err.(*retry.ContextError); ok || err == context.Canceled || err == context.DeadlineExceeded {
		return err
	}
	return gcerr.New(ec.ErrorCode(err), err, 2, "pubsub")
}
