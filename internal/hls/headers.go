package hls

import (
	"fmt"
	"net/http"
	"strings"
)

func ParseHeaders(raw string) (http.Header, error) {
	h := make(http.Header)
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("invalid HTTP header %q; expected Name: Value", line)
		}
		h.Set(strings.TrimSpace(name), strings.TrimSpace(value))
	}
	return h, nil
}
