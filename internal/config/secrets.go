package config

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"
)

func MaskValue(value string) string {
	runes := []rune(value)
	if len(runes) <= 5 {
		return "••••"
	}
	return "••••" + string(runes[len(runes)-5:])
}

func RedactURL(raw string) string {
	clean, pool := ParseProxyEntry(raw)
	if clean != "" {
		raw = clean
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid>"
	}
	if u.User != nil {
		u.User = url.User("***")
	}
	if pool && u.Fragment == "" {
		u.Fragment = "pool"
	}
	return u.String()
}

type SecretRedactor struct {
	values atomic.Value
}

func NewSecretRedactor() *SecretRedactor {
	r := &SecretRedactor{}
	r.values.Store([]string(nil))
	return r
}

func (r *SecretRedactor) Replace(cfg Config) {
	values := make([]string, 0, len(cfg.ServerKeys)+len(cfg.RuntimeProxies())*2+4)
	values = append(values, cfg.ServerKeys...)
	for _, value := range []string{cfg.Upstream.Zen, cfg.Upstream.Anonymous} {
		if parsed, err := url.Parse(value); err == nil && parsed.User != nil {
			values = append(values, value, parsed.User.Username())
			if password, ok := parsed.User.Password(); ok {
				values = append(values, password)
			}
		}
	}
	for _, value := range cfg.RuntimeProxies() {
		if value != "direct" {
			clean, _ := ParseProxyEntry(value)
			values = append(values, value, clean)
			if parsed, err := url.Parse(clean); err == nil && parsed.User != nil {
				values = append(values, parsed.User.Username())
				if password, ok := parsed.User.Password(); ok {
					values = append(values, password)
				}
			}
		}
	}
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	r.values.Store(values)
}

func (r *SecretRedactor) String(value string) string {
	secrets, _ := r.values.Load().([]string)
	for _, secret := range secrets {
		if len(secret) >= 4 && strings.Contains(value, secret) {
			value = strings.ReplaceAll(value, secret, "***")
		}
	}
	return value
}

func Fingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:10]
}
