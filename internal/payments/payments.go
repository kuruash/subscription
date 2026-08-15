// Package payments is the ONLY package allowed to import stripe-go.
//
// Same architectural instinct as internal/repository (the only SQL layer)
// and internal/auth (the only JWT layer): put the third-party SDK behind
// a small, domain-shaped interface so the rest of the app depends on OUR
// types, not Stripe's. Concrete benefits:
//   - Services and handlers stay unit-testable with a fake Client that
//     doesn't need network access or an API key.
//   - Swapping providers (Stripe → Braintree, or a mock for load tests)
//     touches only this file, not the service layer.
//   - The blast radius of a stripe-go major-version bump is confined to
//     this package — if v79→v80 renames a field, we fix it here once.
//
// What we deliberately do NOT do: pass stripe.Event, stripe.PaymentIntent,
// or any other stripe.* type across the package boundary. Those live and
// die in here; callers get PaymentIntent / Event, defined below.
package payments

import (
	"context"
	"errors"
	"fmt"

	"github.com/stripe/stripe-go/v79"
	"github.com/stripe/stripe-go/v79/paymentintent"
	"github.com/stripe/stripe-go/v79/webhook"
)

// ErrInvalidSignature is returned by VerifyWebhookSignature when the
// signature header doesn't match the payload under our webhook secret.
// A sentinel error (instead of just returning the raw stripe error) lets
// the webhook handler map it to a clean 400 with errors.Is, without
// needing to know anything about stripe internals.
var ErrInvalidSignature = errors.New("invalid stripe webhook signature")

// PaymentIntent is our domain-shaped view of a Stripe PaymentIntent.
// Only the fields we actually use downstream. ClientSecret is what gets
// returned to the browser so Stripe.js can confirm the payment client-
// side without our server ever touching the card details.
type PaymentIntent struct {
	ID           string
	ClientSecret string
}

// EventType is our own enum, not stripe.EventType.
// Mapping Stripe's string event names into a closed set of ours means
// the webhook handler can switch on a value it fully controls, and any
// event type we don't care about collapses to EventUnhandled — the
// handler doesn't need a default branch that quietly ignores things.
type EventType string

const (
	EventPaymentSucceeded EventType = "payment_intent.succeeded"
	EventPaymentFailed    EventType = "payment_intent.payment_failed"
	EventUnhandled        EventType = "unhandled"
)

// Event is the parsed, signature-verified webhook payload in domain shape.
//
//   - ID is the Stripe event id (evt_...). This is the idempotency key we
//     store in transactions.stripe_event_id — Stripe guarantees it's
//     stable across redeliveries of the same logical event.
//   - PaymentIntentID (pi_...) is pulled out of the event's data.object
//     so callers don't have to reach into a generic JSON blob.
type Event struct {
	ID              string
	Type            EventType
	PaymentIntentID string
}

// Client is the interface services depend on. Keeping this small (two
// methods) is intentional — every method here is a new place a fake has
// to implement in tests.
type Client interface {
	CreatePaymentIntent(ctx context.Context, amountCents int64, currency string, metadata map[string]string) (*PaymentIntent, error)
	VerifyWebhookSignature(payload []byte, sigHeader string) (*Event, error)
}

// stripeClient is the real, network-talking implementation.
type stripeClient struct {
	webhookSecret string
}

// NewClient wires up the Stripe SDK.
//
// stripe.Key is a PACKAGE-LEVEL GLOBAL in stripe-go — setting it here
// configures every subsequent paymentintent.New() call in the process.
// This is ugly (global state), but it's the SDK's design; the alternative
// (constructing a *client.API and threading it through every call) is
// more verbose without buying us anything at this scale. The whole point
// of isolating stripe-go inside this package is that this global is
// invisible outside of it.
func NewClient(apiKey, webhookSecret string) Client {
	stripe.Key = apiKey
	return &stripeClient{webhookSecret: webhookSecret}
}

// CreatePaymentIntent asks Stripe to create a PaymentIntent — a server-
// side handle for "we want to collect $X from someone." Stripe returns an
// id (pi_...) plus a client_secret; the frontend confirms the payment
// using the client_secret, and Stripe pings our webhook when it
// eventually succeeds or fails.
//
// Why amount is int64 cents, not float64 dollars: money in floats is a
// bug waiting to happen (4.99 is not representable exactly in binary
// float). Stripe's API takes cents, and we match. Callers convert once
// at the boundary (e.g. int64(4.99 * 100) = 499).
func (c *stripeClient) CreatePaymentIntent(ctx context.Context, amountCents int64, currency string, metadata map[string]string) (*PaymentIntent, error) {
	params := &stripe.PaymentIntentParams{
		Amount:   stripe.Int64(amountCents),
		Currency: stripe.String(currency),
		// AutomaticPaymentMethods lets Stripe pick whatever payment methods
		// are enabled on the account — cards, wallets, etc. Simplest thing
		// that works for a sandbox; a real product might pin specific
		// methods per market.
		AutomaticPaymentMethods: &stripe.PaymentIntentAutomaticPaymentMethodsParams{
			Enabled: stripe.Bool(true),
		},
	}
	// stripe.Params.Metadata is a map[string]string that flows through to
	// the webhook payload — we use it to stash our subscription_id so the
	// webhook can find the right row even without a DB lookup by pi_id.
	for k, v := range metadata {
		params.AddMetadata(k, v)
	}
	// The Stripe SDK's Context lives on params, not as a separate arg.
	// Setting it lets an HTTP cancel propagate down to the Stripe request.
	params.Context = ctx

	pi, err := paymentintent.New(params)
	if err != nil {
		return nil, fmt.Errorf("stripe create payment intent: %w", err)
	}
	return &PaymentIntent{
		ID:           pi.ID,
		ClientSecret: pi.ClientSecret,
	}, nil
}

// VerifyWebhookSignature checks the Stripe-Signature header against the
// raw request body using our webhook secret, then parses the event.
//
// CRITICAL: the payload MUST be the EXACT bytes Stripe sent — no
// re-encoding, no whitespace changes, no JSON round-trip. The signature
// is computed over the raw bytes; any modification invalidates it. This
// is why file 7 (main.go) has to preserve Gin's raw body carefully.
//
// webhook.ConstructEvent does the HMAC-SHA256 verification for us AND
// parses the JSON — one call, one failure mode. If the signature is
// bad we return our sentinel ErrInvalidSignature so the handler can
// return 400 without leaking anything about why.
func (c *stripeClient) VerifyWebhookSignature(payload []byte, sigHeader string) (*Event, error) {
	stripeEvent, err := webhook.ConstructEvent(payload, sigHeader, c.webhookSecret)
	if err != nil {
		// Log the specific reason once we have proper logging; for now
		// collapse all signature failures into one sentinel because a
		// well-behaved client (Stripe) will never trigger this — anything
		// that gets here is either misconfigured or hostile, and either
		// way "400: bad signature" is the whole response.
		return nil, fmt.Errorf("%w: %v", ErrInvalidSignature, err)
	}

	evt := &Event{
		ID:   stripeEvent.ID,
		Type: EventUnhandled,
	}

	// Only the two event types we actually care about get parsed further.
	// Anything else is left as EventUnhandled and the handler will ack
	// 200 without doing DB work — Stripe delivers ALL enabled events, and
	// silently ignoring the ones we don't handle is the correct behavior
	// (returning non-2xx would make Stripe retry them forever).
	switch stripeEvent.Type {
	case "payment_intent.succeeded":
		evt.Type = EventPaymentSucceeded
	case "payment_intent.payment_failed":
		evt.Type = EventPaymentFailed
	default:
		return evt, nil
	}

	// Both event types have a PaymentIntent as their data.object.
	// stripeEvent.Data.Object is a map[string]interface{}; the "id" key
	// holds the pi_... string we need. Reaching into it here (rather than
	// unmarshalling into a stripe.PaymentIntent) keeps the dependency on
	// stripe-go types minimal.
	if id, ok := stripeEvent.Data.Object["id"].(string); ok {
		evt.PaymentIntentID = id
	} else {
		return nil, fmt.Errorf("stripe %s event missing payment intent id", stripeEvent.Type)
	}

	return evt, nil
}
