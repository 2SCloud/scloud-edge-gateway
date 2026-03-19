package runtime

import (
	"github.com/valyala/fasthttp"
)

// BuildWafRequestFasthttp construit la map de requête pour le module WAF
// à partir d'un *fasthttp.Request (Fiber).
// La structure est identique à BuildWafRequest — le module WASM ne voit aucune différence.
func BuildWafRequestFasthttp(req *fasthttp.Request) map[string]any {
	uri := req.URI()

	h := map[string][]string{}
	req.Header.VisitAll(func(k, v []byte) {
		key := string(k)
		h[key] = append(h[key], string(v))
	})

	q := map[string][]string{}
	uri.QueryArgs().VisitAll(func(k, v []byte) {
		key := string(k)
		q[key] = append(q[key], string(v))
	})

	scheme := string(uri.Scheme())
	if scheme == "" {
		scheme = "http"
	}

	// L'IP réelle n'est pas dans *fasthttp.Request (elle est dans RequestCtx).
	// On lit X-Real-IP puis X-Forwarded-For en fallback.
	// Pour l'IP de connexion brute, enrichir avec c.IP() dans le handler.
	ip := string(req.Header.PeekBytes([]byte("X-Real-IP")))
	if ip == "" {
		ip = string(req.Header.PeekBytes([]byte("X-Forwarded-For")))
	}

	return map[string]any{
		"path":        string(uri.Path()),
		"raw_path":    string(uri.PathOriginal()),
		"method":      string(req.Header.Method()),
		"host":        string(uri.Host()),
		"scheme":      scheme,
		"ip":          ip,
		"remote_addr": ip,
		"user_agent":  string(req.Header.UserAgent()),
		"referer":     string(req.Header.Referer()),
		"headers":     h,
		"query":       q,
	}
}

// BuildRateLimitRequestFasthttp construit la map de requête pour le module RateLimit
// à partir d'un *fasthttp.Request (Fiber).
func BuildRateLimitRequestFasthttp(req *fasthttp.Request) map[string]any {
	uri := req.URI()

	ip := string(req.Header.PeekBytes([]byte("X-Real-IP")))
	if ip == "" {
		ip = string(req.Header.PeekBytes([]byte("X-Forwarded-For")))
	}

	return map[string]any{
		"ip":     ip,
		"path":   string(uri.Path()),
		"method": string(req.Header.Method()),
	}
}
