// stripe_webhook_handler.go — receives POST /webhooks/stripe from Stripe.
//
// SECURITY:
// The webhook is a PUBLIC endpoint — no JWT — because the caller is
// Stripe, not a logged-in user. That makes signature verification the
// ONLY thing standing between us and a forged "the customer paid"
// message from a random attacker. So we verify BEFORE we trust ANY
// field of the payload: 400 on bad signature, no DB writes, no logs
// that leak payload contents.
//
// IDEMPOTENCY:
// Stripe retries webhook deliveries on any non-2xx response, with
// exponential backoff, for up to ~3 days. That means:
//   - A network hiccup on our side must NOT cause double-processing.
//     Handled by transactions.stripe_event_id UNIQUE — a duplicate
//     insert returns ErrDuplicateEvent, which we translate to 200 so
//     Stripe stops retrying.
//   - An unhandled event type (Stripe delivers ALL enabled events, not
//     just the ones we opted into) must return 200 too, or Stripe
//     retries forever for events we deliberately ignore.
//   - A genuine failure (DB down, code panics) SHOULD return 5xx so
//     Stripe retries. We only 2xx when the event has been recorded or
//     confidently doesn't need recording.
package handlers

import (
	"errors"
	"io"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"

	"subscription-service/internal/payments"
	"subscription-service/internal/services"
)

// StripeWebhookHandler owns the /webhooks/stripe endpoint. Depends only
// on the payments client (for signature verification) and the service
// (for state changes) — the SDK stays behind those interfaces, so this
// handler doesn't import stripe-go directly.
type StripeWebhookHandler struct {
	svc       *services.SubscriptionService
	payClient payments.Client
}

func NewStripeWebhookHandler(svc *services.SubscriptionService, payClient payments.Client) *StripeWebhookHandler {
	return &StripeWebhookHandler{svc: svc, payClient: payClient}
}

// Register attaches the route to the given router. Caller (main.go)
// MUST attach this to a router group WITHOUT the JWT middleware — see
// the SECURITY comment above.
func (h *StripeWebhookHandler) Register(r gin.IRouter) {
	r.POST("/webhooks/stripe", h.handle)
}

// PaymentAmountCents is the amount recorded on the transactions row
// when a payment_intent.succeeded is processed. Ideally we'd read the
// actual amount from the Stripe event payload; this is a Phase 7
// simplification and matches the price we quoted at Subscribe time
// (monthly = $4.99 = 499 cents). Documented gap: fix when we support
// mixed plans or promo codes where the charged amount can differ from
// the plan's list price.
const PaymentAmountCents int64 = 499

func (h *StripeWebhookHandler) handle(c *gin.Context) {
	// 1. Read the raw body. Signature verification is computed over the
	//    EXACT bytes Stripe sent — no re-encoding, no whitespace changes.
	//    File 7 (main.go) is responsible for making sure Gin hasn't
	//    already consumed or mutated the body before we get here.
	payload, err := io.ReadAll(c.Request.Body)
	if err != nil {
		// If we can't even read the body, we can't verify the signature.
		// 400 is correct — this is a malformed request, not a server bug.
		c.JSON(http.StatusBadRequest, gin.H{"error": "read body"})
		return
	}

	// 2. Verify the signature. Any failure = 400, no logs of payload
	//    content (attacker could otherwise probe our behavior via logs).
	sigHeader := c.GetHeader("Stripe-Signature")
	event, err := h.payClient.VerifyWebhookSignature(payload, sigHeader)
	if err != nil {
		if errors.Is(err, payments.ErrInvalidSignature) {
			// Deliberately terse — don't tell an attacker what went wrong.
			log.Printf("webhook: signature verification failed")
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid signature"})
			return
		}
		// Other errors from the payments layer (e.g. malformed event
		// missing the payment intent id) — 400 as well; there's nothing
		// Stripe can do by retrying a payload we can't parse.
		log.Printf("webhook: parse error: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad payload"})
		return
	}

	// 3. Dispatch on event type. Everything past this point is trusted
	//    payload — signature verification proved it came from Stripe.
	switch event.Type {
	case payments.EventPaymentSucceeded:
		h.handleSucceeded(c, event)
	case payments.EventPaymentFailed:
		h.handleFailed(c, event)
	default:
		// Unhandled event type. Stripe delivers every enabled event; if
		// we return non-2xx for an event we simply chose not to handle,
		// Stripe retries it forever. 200 with a log is the correct ack.
		log.Printf("webhook: ignoring event %s (id=%s)", event.Type, event.ID)
		c.Status(http.StatusOK)
	}
}

func (h *StripeWebhookHandler) handleSucceeded(c *gin.Context, event *payments.Event) {
	// The service returns ErrDuplicateEvent iff the DB's
	// stripe_event_id UNIQUE index rejected the transaction insert —
	// i.e. we've seen this event id before. That means the FIRST
	// delivery already succeeded end-to-end (transaction row written,
	// subscription flipped to active, notification published). Ack 200
	// so Stripe stops retrying.
	sub, err := h.svc.MarkPaymentSucceeded(c.Request.Context(), event.PaymentIntentID, event.ID, PaymentAmountCents)
	switch {
	case err == nil:
		log.Printf("webhook: payment_intent.succeeded processed pi=%s event=%s sub=%d", event.PaymentIntentID, event.ID, sub.ID)
		c.Status(http.StatusOK)
	case errors.Is(err, services.ErrDuplicateEvent):
		log.Printf("webhook: payment_intent.succeeded already processed event=%s (idempotent ack)", event.ID)
		c.Status(http.StatusOK)
	case errors.Is(err, services.ErrNotFound):
		// The PaymentIntent doesn't map to any subscription in our DB.
		// This is a data-integrity signal (someone else's Stripe
		// account? Manually-cancelled sub row? Test event without a
		// matching sub?), not something Stripe can fix by retrying.
		// Log loudly and ack 200 so we don't create a retry storm for
		// an event we cannot process.
		log.Printf("webhook: WARN payment_intent.succeeded for unknown pi=%s event=%s — acking to stop retries", event.PaymentIntentID, event.ID)
		c.Status(http.StatusOK)
	default:
		// Genuine failure (DB unreachable, unexpected error). 500 tells
		// Stripe to retry — the event has NOT been recorded and we
		// want another shot at it.
		log.Printf("webhook: ERROR processing payment_intent.succeeded pi=%s event=%s: %v", event.PaymentIntentID, event.ID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
	}
}

func (h *StripeWebhookHandler) handleFailed(c *gin.Context, event *payments.Event) {
	_, err := h.svc.MarkPaymentFailed(c.Request.Context(), event.PaymentIntentID)
	switch {
	case err == nil:
		log.Printf("webhook: payment_intent.payment_failed processed pi=%s event=%s", event.PaymentIntentID, event.ID)
		c.Status(http.StatusOK)
	case errors.Is(err, services.ErrNotFound):
		// No pending row for this PI. Reasons:
		//   - The user already cancelled the sub manually before Stripe
		//     reported the failure.
		//   - payment_succeeded already flipped it to active (unusual
		//     but possible if events arrive out of order).
		//   - We never had it (foreign PaymentIntent).
		// None of these are recoverable by retry — ack 200.
		log.Printf("webhook: payment_intent.payment_failed for pi=%s but no pending row (already handled?) — acking", event.PaymentIntentID)
		c.Status(http.StatusOK)
	default:
		log.Printf("webhook: ERROR processing payment_intent.payment_failed pi=%s event=%s: %v", event.PaymentIntentID, event.ID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
	}
}
