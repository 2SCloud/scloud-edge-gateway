// Package proxy implements the DNS-over-HTTPS (DoH) handler that turns
// RFC 8484 HTTPS requests hitting /dns-query on the edge gateway into
// plain UDP/TCP DNS queries against an internal authoritative resolver.
//
// Why the gateway translates instead of the DNS server speaking DoH:
//
//   - The gateway is already the only public entrypoint. Putting DoH on
//     the gateway keeps the public attack surface on one process.
//   - scloud-dns stays a pure UDP/TCP DNS server. No TLS config, no cert
//     rotation, no extra listener. Fewer moving parts per service.
//   - The WAF and rate-limit modules in front of this handler apply to
//     every DoH query automatically — same request pipeline as the rest
//     of the HTTP traffic, same logs, same metrics.
//   - This is how Cloudflare, Google, Quad9 and other production DoH
//     providers are built: edge terminates DoH, back end stays UDP.
package proxy

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"sync"
	"time"

	"2scloud-edge-gateway/internal/utils"

	"github.com/gofiber/fiber/v2"
	"github.com/miekg/dns"
)

// Media type defined by RFC 8484 §6. Clients use it on requests and
// must receive it on successful responses. Anything else is a protocol
// error from the gateway's perspective.
const DoHMediaType = "application/dns-message"

// RFC 8484 §6: the ?dns= parameter on a GET is base64url-encoded
// (no padding) of the DNS wire-format query.
var dohB64 = base64.RawURLEncoding

// Config describes the DoH proxy's behavior. The zero value is invalid —
// UpstreamAddr must always be set. Timeouts and size limits fall back to
// sane defaults when left at zero.
type Config struct {
	// UpstreamAddr is the "host:port" of the backing DNS resolver.
	// Example: "scloud-dns.scloud-dns.svc.cluster.local:53".
	UpstreamAddr string

	// QueryTimeout caps how long a single upstream exchange may take.
	// Defaults to 2s if unset.
	QueryTimeout time.Duration

	// MaxMessageSize bounds both the DNS query and the DNS response in
	// bytes. Defaults to 65535 (TCP DNS ceiling) when unset. Well-formed
	// RFC 8484 messages are normally well under 1500 bytes.
	MaxMessageSize int
}

// DoH is a reusable handler. It keeps persistent UDP and TCP dns.Client
// instances so sockets are reused across requests.
type DoH struct {
	cfg       Config
	udpClient *dns.Client
	tcpClient *dns.Client

	// mu guards the rare config-mutation path (future hot reload).
	mu sync.RWMutex
}

// New returns a DoH handler ready to serve traffic. It does not dial
// the upstream — that happens lazily on the first request.
func New(cfg Config) (*DoH, error) {
	if cfg.UpstreamAddr == "" {
		return nil, fmt.Errorf("doh: upstream_addr is required")
	}
	if cfg.QueryTimeout <= 0 {
		cfg.QueryTimeout = 2 * time.Second
	}
	if cfg.MaxMessageSize <= 0 {
		cfg.MaxMessageSize = dns.MaxMsgSize
	}

	return &DoH{
		cfg: cfg,
		udpClient: &dns.Client{
			Net:     "udp",
			Timeout: cfg.QueryTimeout,
			UDPSize: dns.DefaultMsgSize,
		},
		tcpClient: &dns.Client{
			Net:     "tcp",
			Timeout: cfg.QueryTimeout,
		},
	}, nil
}

// Handler returns the Fiber handler that implements RFC 8484 on
// /dns-query. Callers are expected to mount it at the exact path
// "/dns-query" — no prefix matching.
func (d *DoH) Handler() fiber.Handler {
	return func(c *fiber.Ctx) error {
		query, err := d.extractQuery(c)
		if err != nil {
			// Distinguish between "client spoke bad DoH" (400) and
			// "method not supported" (405). Both are protocol errors
			// visible on the wire, not upstream issues.
			if err == errMethodNotAllowed {
				c.Set("Allow", "GET, POST")
				return c.Status(fiber.StatusMethodNotAllowed).
					SendString("method not allowed: DoH accepts GET or POST")
			}
			utils.LogWarning("DoH: malformed request from %s: %v", c.IP(), err)
			return c.Status(fiber.StatusBadRequest).
				SendString(fmt.Sprintf("malformed DoH request: %v", err))
		}

		// Parse once so we can log the qname/qtype. Parse errors here
		// are a client bug (bad wire format) — surface as 400.
		reqMsg := new(dns.Msg)
		if err := reqMsg.Unpack(query); err != nil {
			utils.LogWarning("DoH: unpack query failed from %s: %v", c.IP(), err)
			return c.Status(fiber.StatusBadRequest).
				SendString("invalid DNS wire format in query")
		}
		qname, qtype := describeQuestion(reqMsg)

		resp, err := d.exchange(reqMsg)
		if err != nil {
			utils.LogWarning("DoH: upstream failure for qname=%s qtype=%s client=%s: %v",
				qname, qtype, c.IP(), err)
			return c.Status(fiber.StatusBadGateway).
				SendString(fmt.Sprintf("upstream DNS failure: %v", err))
		}

		wire, err := resp.Pack()
		if err != nil {
			utils.LogError("DoH: pack response failed qname=%s: %v", qname, err)
			return c.Status(fiber.StatusInternalServerError).
				SendString("failed to encode DNS response")
		}
		if len(wire) > d.cfg.MaxMessageSize {
			utils.LogWarning("DoH: response too large qname=%s size=%d limit=%d",
				qname, len(wire), d.cfg.MaxMessageSize)
			return c.Status(fiber.StatusInternalServerError).
				SendString("DNS response exceeds configured size limit")
		}

		// RFC 8484 §5.1: Cache-Control max-age MUST NOT exceed the
		// minimum TTL in the DNS response. Zero is fine (means don't
		// cache) so NXDOMAIN/SERVFAIL don't get pinned in caches.
		minTTL := minResponseTTL(resp)

		utils.LogInfo("DoH qname=%s qtype=%s rcode=%s answers=%d ttl=%ds client=%s bytes_in=%d bytes_out=%d",
			qname, qtype, dns.RcodeToString[resp.Rcode],
			len(resp.Answer), minTTL, c.IP(), len(query), len(wire))

		c.Set(fiber.HeaderContentType, DoHMediaType)
		c.Set("Cache-Control", fmt.Sprintf("max-age=%d", minTTL))
		// Hint to any intermediate cache that two requests differ only
		// by body content, not headers.
		c.Set("Vary", "Accept")
		return c.Status(fiber.StatusOK).Send(wire)
	}
}

// ─── Internal helpers ─────────────────────────────────────────────────────────

var errMethodNotAllowed = fmt.Errorf("method not allowed")

// extractQuery pulls the DNS wire-format query bytes out of the HTTP
// request according to RFC 8484 §4.1 (GET ?dns=) or §4.2 (POST body).
func (d *DoH) extractQuery(c *fiber.Ctx) ([]byte, error) {
	method := c.Method()

	switch method {
	case http.MethodGet:
		raw := c.Query("dns")
		if raw == "" {
			return nil, fmt.Errorf("missing required 'dns' query parameter")
		}
		query, err := dohB64.DecodeString(raw)
		if err != nil {
			return nil, fmt.Errorf("'dns' parameter is not valid base64url: %w", err)
		}
		if len(query) > d.cfg.MaxMessageSize {
			return nil, fmt.Errorf("query size %d exceeds limit %d", len(query), d.cfg.MaxMessageSize)
		}
		return query, nil

	case http.MethodPost:
		ct := c.Get(fiber.HeaderContentType)
		if ct != DoHMediaType {
			return nil, fmt.Errorf("Content-Type must be %q, got %q", DoHMediaType, ct)
		}
		body := c.Body()
		if len(body) == 0 {
			return nil, fmt.Errorf("empty DoH request body")
		}
		if len(body) > d.cfg.MaxMessageSize {
			return nil, fmt.Errorf("query size %d exceeds limit %d", len(body), d.cfg.MaxMessageSize)
		}
		// fiber's Body() returns a slice into the request buffer; copy
		// so the upstream client doesn't race with request reuse.
		out := make([]byte, len(body))
		copy(out, body)
		return out, nil

	default:
		return nil, errMethodNotAllowed
	}
}

// exchange sends the query upstream over UDP, falling back to TCP when
// the server signals truncation (TC bit, RFC 1035 §4.2.1).
func (d *DoH) exchange(msg *dns.Msg) (*dns.Msg, error) {
	d.mu.RLock()
	upstream := d.cfg.UpstreamAddr
	udp := d.udpClient
	tcp := d.tcpClient
	d.mu.RUnlock()

	resp, _, err := udp.Exchange(msg, upstream)
	if err != nil {
		return nil, fmt.Errorf("udp exchange: %w", err)
	}
	if resp == nil {
		return nil, fmt.Errorf("udp exchange: nil response")
	}
	if resp.Truncated {
		resp, _, err = tcp.Exchange(msg, upstream)
		if err != nil {
			return nil, fmt.Errorf("tcp fallback exchange: %w", err)
		}
		if resp == nil {
			return nil, fmt.Errorf("tcp fallback exchange: nil response")
		}
	}
	return resp, nil
}

// minResponseTTL returns the smallest TTL across Answer, Ns and Extra
// sections, clamped to [0, 3600]. Used for Cache-Control max-age.
func minResponseTTL(msg *dns.Msg) uint32 {
	var min uint32 = 3600
	seen := false
	for _, rrs := range [][]dns.RR{msg.Answer, msg.Ns, msg.Extra} {
		for _, rr := range rrs {
			// OPT records carry EDNS0 state, not a real TTL — skip.
			if _, isOPT := rr.(*dns.OPT); isOPT {
				continue
			}
			ttl := rr.Header().Ttl
			if !seen || ttl < min {
				min = ttl
				seen = true
			}
		}
	}
	if !seen {
		return 0
	}
	if min > 3600 {
		return 3600
	}
	return min
}

// describeQuestion returns a "qname / qtype" pair suitable for logs.
// Returns placeholders when the message has no question (spec-legal
// but unusual — some probing traffic does this).
func describeQuestion(msg *dns.Msg) (qname, qtype string) {
	if len(msg.Question) == 0 {
		return "<empty>", "<empty>"
	}
	q := msg.Question[0]
	qname = q.Name
	qtype = dns.TypeToString[q.Qtype]
	if qtype == "" {
		qtype = fmt.Sprintf("TYPE%d", q.Qtype)
	}
	return qname, qtype
}

