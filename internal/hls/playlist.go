package hls

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

func parseMaster(data []byte, base *url.URL, maxHeight int) (string, bool, error) {
	s := bufio.NewScanner(bytes.NewReader(data))
	var next *variant
	var choices []variant
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			attrs := parseAttrs(strings.TrimPrefix(line, "#EXT-X-STREAM-INF:"))
			v := variant{}
			v.Bandwidth, _ = strconv.ParseInt(attrs["BANDWIDTH"], 10, 64)
			if res := strings.SplitN(attrs["RESOLUTION"], "x", 2); len(res) == 2 {
				v.Height, _ = strconv.Atoi(res[1])
			}
			next = &v
			continue
		}
		if next != nil && line != "" && !strings.HasPrefix(line, "#") {
			ref, err := base.Parse(line)
			if err != nil {
				return "", true, err
			}
			next.URI = ref.String()
			choices = append(choices, *next)
			next = nil
		}
	}
	if err := s.Err(); err != nil {
		return "", true, err
	}
	if len(choices) == 0 {
		return "", false, nil
	}
	eligible := choices[:0]
	for _, v := range choices {
		if maxHeight <= 0 || v.Height == 0 || v.Height <= maxHeight {
			eligible = append(eligible, v)
		}
	}
	if len(eligible) == 0 {
		return "", true, fmt.Errorf("no HLS variant is at or below max height %d", maxHeight)
	}
	best := eligible[0]
	for _, v := range eligible[1:] {
		if v.Bandwidth > best.Bandwidth {
			best = v
		}
	}
	return best.URI, true, nil
}

func parseMedia(data []byte, base *url.URL) (playlist, error) {
	var p playlist
	s := bufio.NewScanner(bytes.NewReader(data))
	s.Buffer(make([]byte, 4096), 2*1024*1024)
	sequence := uint64(0)
	var pendingRange string
	var previousRangeEnd int64
	var previousRangeURI string
	var currentKey *keyInfo
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		switch {
		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
			n, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:")), 10, 64)
			if err != nil {
				return p, fmt.Errorf("invalid media sequence: %w", err)
			}
			sequence = n
		case strings.HasPrefix(line, "#EXT-X-KEY:"):
			attrs := parseAttrs(strings.TrimPrefix(line, "#EXT-X-KEY:"))
			if strings.EqualFold(attrs["METHOD"], "NONE") {
				currentKey = nil
				continue
			}
			if !strings.EqualFold(attrs["METHOD"], "AES-128") {
				return p, fmt.Errorf("unsupported HLS encryption method %q", attrs["METHOD"])
			}
			keyURL, err := base.Parse(attrs["URI"])
			if err != nil || attrs["URI"] == "" {
				return p, fmt.Errorf("invalid AES-128 key URI")
			}
			k := &keyInfo{URI: keyURL.String()}
			if iv := strings.TrimPrefix(strings.ToLower(attrs["IV"]), "0x"); iv != "" {
				if len(iv)%2 != 0 {
					iv = "0" + iv
				}
				decoded, err := hex.DecodeString(iv)
				if err != nil || len(decoded) > 16 {
					return p, fmt.Errorf("invalid AES IV")
				}
				k.IV = make([]byte, 16)
				copy(k.IV[16-len(decoded):], decoded)
			}
			currentKey = k
		case strings.HasPrefix(line, "#EXT-X-BYTERANGE:"):
			pendingRange = strings.TrimSpace(strings.TrimPrefix(line, "#EXT-X-BYTERANGE:"))
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			attrs := parseAttrs(strings.TrimPrefix(line, "#EXT-X-MAP:"))
			u, err := base.Parse(attrs["URI"])
			if err != nil || attrs["URI"] == "" {
				return p, fmt.Errorf("invalid EXT-X-MAP URI")
			}
			p.InitURI = u.String()
		case line == "#EXT-X-ENDLIST":
			p.EndList = true
		case line != "" && !strings.HasPrefix(line, "#"):
			u, err := base.Parse(line)
			if err != nil {
				return p, err
			}
			seg := segment{URI: u.String(), Sequence: sequence, Key: cloneKey(currentKey)}
			if pendingRange != "" {
				lengthText, offsetText, hasOffset := strings.Cut(pendingRange, "@")
				length, err := strconv.ParseInt(lengthText, 10, 64)
				if err != nil || length <= 0 {
					return p, fmt.Errorf("invalid HLS byte range %q", pendingRange)
				}
				start := previousRangeEnd
				if hasOffset {
					start, err = strconv.ParseInt(offsetText, 10, 64)
					if err != nil || start < 0 {
						return p, fmt.Errorf("invalid HLS byte range offset %q", pendingRange)
					}
				} else if previousRangeURI != u.String() {
					return p, fmt.Errorf("implicit byte range offset requires consecutive ranges from the same URI")
				}
				seg.RangeStart, seg.RangeLength, seg.HasRange = start, length, true
				previousRangeEnd = start + length
				previousRangeURI = u.String()
			} else {
				previousRangeEnd = 0
				previousRangeURI = ""
			}
			p.Segments = append(p.Segments, seg)
			sequence++
			pendingRange = ""
		}
	}
	if err := s.Err(); err != nil {
		return p, err
	}
	if len(p.Segments) == 0 {
		return p, fmt.Errorf("playlist contains no media segments")
	}
	return p, nil
}

func cloneKey(k *keyInfo) *keyInfo {
	if k == nil {
		return nil
	}
	c := *k
	c.IV = append([]byte(nil), k.IV...)
	return &c
}

func parseAttrs(s string) map[string]string {
	out := map[string]string{}
	var b strings.Builder
	quoted := false
	parts := []string{}
	for _, r := range s {
		if r == '"' {
			quoted = !quoted
		}
		if r == ',' && !quoted {
			parts = append(parts, b.String())
			b.Reset()
			continue
		}
		b.WriteRune(r)
	}
	parts = append(parts, b.String())
	for _, p := range parts {
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), "\"")
	}
	return out
}
