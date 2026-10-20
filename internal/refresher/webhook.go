package refresher

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

// SignatureHeader carries the signature of a webhook payload.
//
// The value is "sha256=" followed by the hex-encoded HMAC-SHA256 of the
// raw request body, keyed with the shared secret. A receiver recomputes it
// and compares the two in constant time; a mismatch means the payload was
// tampered with, or was not sent by SoroVault at all.
const SignatureHeader = "X-SoroVault-Signature"

// DefaultWebhookTimeout bounds one delivery.
//
// The webhook is a courtesy to whoever wants to know about an upgrade, not
// part of the sweep: a receiver that hangs must not hold the sweep up.
const DefaultWebhookTimeout = 5 * time.Second

// Change describes a contract whose interface changed.
type Change struct {
	// ContractID is the contract that changed.
	ContractID string `json:"contract_id"`
	// Network is the network the contract lives on.
	Network string `json:"network"`
	// OldWasmHash is the hash the contract ran before the change.
	OldWasmHash string `json:"old_wasm_hash"`
	// NewWasmHash is the hash it runs now.
	NewWasmHash string `json:"new_wasm_hash"`
	// InterfaceURL is where the new interface can be read. It is a path
	// relative to the SoroVault instance, since the instance does not know
	// the URL it is served under.
	InterfaceURL string `json:"interface_url"`
	// ChangedAt is when the change was noticed.
	ChangedAt time.Time `json:"changed_at"`
}

// Notifier is told about contracts whose interface changed.
//
// Delivery is best effort: a notifier that fails is logged and otherwise
// ignored, because the sweep's job is to keep the registry up to date.
type Notifier interface {
	Notify(ctx context.Context, change Change) error
}

// Webhook posts changes to an HTTP endpoint as JSON.
type Webhook struct {
	url    string
	secret string
	client *http.Client
	log    *slog.Logger
	now    func() time.Time
}

var _ Notifier = (*Webhook)(nil)

// WebhookOptions configures a Webhook.
type WebhookOptions struct {
	// URL is the endpoint changes are posted to. It must be an absolute
	// HTTP or HTTPS URL.
	URL string
	// Secret, when set, signs each payload with HMAC-SHA256.
	Secret string
	// Timeout bounds one delivery. Zero means DefaultWebhookTimeout.
	Timeout time.Duration
	// Logger receives delivery failures. Defaults to slog.Default.
	Logger *slog.Logger
	// Client overrides the HTTP client. Tests use it; production code
	// leaves it alone.
	Client *http.Client
	// Now is the clock. Tests replace it; production code leaves it alone.
	Now func() time.Time
}

// NewWebhook builds a Webhook.
//
// It returns an error for a URL that is not an absolute HTTP or HTTPS
// address: a webhook that cannot be delivered to is a configuration
// mistake, and is better found at startup than on the first upgrade.
func NewWebhook(opts WebhookOptions) (*Webhook, error) {
	if opts.URL == "" {
		return nil, fmt.Errorf("refresher: webhook URL must not be empty")
	}
	parsed, err := url.Parse(opts.URL)
	if err != nil {
		return nil, fmt.Errorf("refresher: webhook URL %q: %w", opts.URL, err)
	}
	if !parsed.IsAbs() || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("refresher: webhook URL %q must be an absolute HTTP or HTTPS URL", opts.URL)
	}

	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultWebhookTimeout
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	return &Webhook{
		url:    opts.URL,
		secret: opts.Secret,
		client: client,
		log:    log,
		now:    now,
	}, nil
}

// Notify posts one change.
//
// The request is made once: there is no retry, because a receiver that is
// down must not be able to stall the sweep. The caller decides what to do
// with the error.
func (w *Webhook) Notify(ctx context.Context, change Change) error {
	if change.ChangedAt.IsZero() {
		change.ChangedAt = w.now().UTC()
	}
	body, err := json.Marshal(change)
	if err != nil {
		return fmt.Errorf("refresher: encoding webhook payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("refresher: building webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "sorovault-refresher")
	if w.secret != "" {
		req.Header.Set(SignatureHeader, Sign(w.secret, body))
	}

	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("refresher: posting webhook: %w", err)
	}
	defer resp.Body.Close()
	// The body is drained, up to a point, so the connection can be reused
	// without trusting the receiver to send a small one.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("refresher: webhook returned %s", resp.Status)
	}

	w.log.DebugContext(ctx, "refresh webhook delivered",
		"contract_id", change.ContractID, "status", resp.StatusCode)
	return nil
}

// Sign returns the value of the signature header for a payload.
//
// It is exported so a receiver written in Go can verify a payload the same
// way SoroVault signs it.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// interfaceURL is where the new interface of a contract can be read.
func interfaceURL(contractID, network, wasmHash string) string {
	values := url.Values{}
	values.Set("network", network)
	if wasmHash != "" {
		values.Set("wasm_hash", wasmHash)
	}
	return "/api/contracts/" + url.PathEscape(contractID) + "?" + values.Encode()
}
