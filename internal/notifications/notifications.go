// Package notifications is an in-process event queue for "something
// happened to a subscription" signals. Producers (the service layer)
// Publish; a single consumer goroutine reads them and calls
// NotifyCreator (currently a log-only stub).
//
// Why a Go channel and not SQS yet:
//   - The interesting *pattern* (producer decouples from consumer so the
//     HTTP handler doesn't block on the notification path) is identical
//     to SQS. Building it with a channel first lets us prove the shape
//     without an AWS account, IAM policy, or region config.
//   - When SQS arrives later, we swap the Queue implementation behind
//     the Publisher interface. Nothing in services or handlers changes.
//
// Big trade this makes: DURABILITY. See doc on Queue.Publish.
package notifications

import (
	"context"
	"log"
)

// EventType is a small enum of what happened.
type EventType string

const (
	EventSubscribed EventType = "subscribed"
	EventExpired    EventType = "expired"
)

// Event is what producers hand to Publish and what the consumer receives.
// It carries only IDs — the consumer can re-fetch full records from the
// DB if it needs more. This keeps events small and cheap to enqueue.
type Event struct {
	SubscriptionID int
	UserID         int
	CreatorID      int
	Type           EventType
}

// Publisher is the interface the service layer depends on. Making it an
// interface (not a concrete *Queue) means a unit test can pass in a fake
// that records events without spinning up any goroutine, and it means the
// SQS swap in a later phase touches only main.go — services stay unchanged.
type Publisher interface {
	Publish(ev Event)
}

// Queue is the channel-backed in-process implementation of Publisher.
type Queue struct {
	ch chan Event
}

// NewQueue returns a Queue with a buffered channel. Buffer size matters:
//   - too small → a burst of subscribes will drop events under load
//   - too large → memory grows unbounded if the consumer stalls
//
// 128 is a comfortable local-dev / low-scale default; production would
// tune this based on measured throughput and switch to SQS anyway.
func NewQueue(buffer int) *Queue {
	if buffer <= 0 {
		buffer = 128
	}
	return &Queue{ch: make(chan Event, buffer)}
}

// Publish is a NON-BLOCKING send. If the buffer is full we drop the event
// and log loudly instead of stalling the caller.
//
// Why non-blocking:
//   - Publish is called from inside an HTTP request handler (via the
//     service layer). Blocking here would make the API's response latency
//     depend on the consumer's health — exactly the coupling this queue
//     exists to prevent.
//
// What "dropped" costs us:
//   - The DB write already committed (we publish AFTER commit). So a
//     dropped event does NOT mean data is inconsistent — the subscription
//     truly exists. It just means the creator won't get a notification
//     for that particular subscribe/expire.
//
// Documented gap (same category as Phase 2's Redis DEL failure and Phase
// 3's worker-restart timing): no retry, no dead-letter queue. Acceptable
// at this stage because:
//  1. The primary system-of-record state (Postgres) is unaffected.
//  2. Under normal load the buffer is never full — dropped events would
//     show up as WARN logs, giving us signal before it becomes a real
//     problem.
//  3. The fix (durable queue, ack/retry semantics) is exactly what SQS
//     gives us for free once we swap the implementation.
func (q *Queue) Publish(ev Event) {
	select {
	case q.ch <- ev:
		// enqueued
	default:
		log.Printf("notifications: DROPPED event %+v (buffer full, capacity=%d)", ev, cap(q.ch))
	}
}

// Consume runs the consumer loop. Call this once in a goroutine from
// main() — running it multiple times would race N consumers over the
// same channel, which is fine for load but overkill here.
//
// The loop exits cleanly when ctx is cancelled. Any events still sitting
// in the buffer at that moment are lost (see durability gap above).
func (q *Queue) Consume(ctx context.Context) {
	log.Printf("notifications: consumer started (capacity=%d)", cap(q.ch))
	for {
		select {
		case <-ctx.Done():
			log.Printf("notifications: consumer stopping (dropped %d in-flight events)", len(q.ch))
			return
		case ev := <-q.ch:
			NotifyCreator(ev)
		}
	}
}

// NotifyCreator is the sink. Currently a log-only stub — swap for a real
// email/push provider in a later phase.
//
// IDEMPOTENCY:
// Right now this is a pure log call, so being invoked twice for the same
// event is harmless — you just see the log line twice. Once it becomes a
// real email send, duplicates would mean duplicate emails, which is
// annoying but not corrupting. The clean fix is a dedupe key on the
// event (e.g. a UUID stamped at Publish time) and consumer-side "have I
// processed this key already?" tracking. Deferred: not worth building
// against a log stub, and SQS's own message dedupe features may handle
// it once we swap.
func NotifyCreator(ev Event) {
	log.Printf("notifications: would notify creator=%d about %s of subscription=%d (user=%d)",
		ev.CreatorID, ev.Type, ev.SubscriptionID, ev.UserID)
}
