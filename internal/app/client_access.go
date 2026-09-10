package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
)

func readClientAccess(path string) ([]netip.Prefix, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open client access config: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(data) > 65536 {
		return nil, fmt.Errorf("cannot read client access config (maximum 64 KiB)")
	}
	var config struct {
		AllowedCIDRs []string `json:"allowed_cidrs"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("invalid client access config: %w", err)
	}
	if config.AllowedCIDRs == nil {
		return nil, fmt.Errorf("client access config requires allowed_cidrs")
	}
	prefixes := make([]netip.Prefix, 0, len(config.AllowedCIDRs))
	for _, cidr := range config.AllowedCIDRs {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			return nil, fmt.Errorf("invalid client CIDR %q: %w", cidr, err)
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return prefixes, nil
}

func clientAccessHandler(path string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Use the socket peer, never spoofable forwarding headers. Read the policy
		// again so edits apply without restarting or interrupting existing streams.
		prefixes, err := readClientAccess(path)
		if err != nil {
			http.Error(w, "client access configuration unavailable", http.StatusServiceUnavailable)
			return
		}
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err == nil {
			if ip, err := netip.ParseAddr(host); err == nil {
				for _, prefix := range prefixes {
					if prefix.Contains(ip.Unmap()) {
						next.ServeHTTP(w, r)
						return
					}
				}
			}
		}
		http.Error(w, "client address is not allowed", http.StatusForbidden)
	})
}
