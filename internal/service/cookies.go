package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const maxCookieInput = 256 << 10

var requiredCookies = []string{"SID", "HSID", "SSID", "OSID", "APISID", "SAPISID"}

// ParseCookies accepts any of:
//   - a "Copy as cURL (bash)" command containing a Cookie header or -b/--cookie
//   - a JSON object of cookie name -> value
//   - a raw Cookie header value ("a=b; c=d", optionally prefixed with "Cookie:")
//
// It returns every cookie it found and validates that the required ones are present.
// Error messages never include cookie values, and callers must not log the input.
func ParseCookies(input string) (map[string]string, error) {
	input = strings.TrimSpace(strings.TrimPrefix(input, "\ufeff"))
	switch {
	case input == "":
		return nil, errors.New("empty input")
	case len(input) > maxCookieInput:
		return nil, errors.New("input too large")
	}

	var (
		cookies map[string]string
		err     error
	)
	switch {
	case strings.HasPrefix(input, "{"):
		cookies, err = parseCookieJSON(input)
	case strings.HasPrefix(strings.ToLower(input), "curl"):
		cookies, err = parseCurlCookies(input)
	default:
		cookies = map[string]string{}
		h := input
		if len(h) >= 7 && strings.EqualFold(h[:7], "cookie:") {
			h = h[7:]
		}
		parseCookieHeader(h, cookies)
	}
	if err != nil {
		return nil, err
	}

	var missing []string
	for _, name := range requiredCookies {
		if cookies[name] == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required cookies: %s", strings.Join(missing, ", "))
	}
	return cookies, nil
}

func parseCookieJSON(s string) (map[string]string, error) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		str, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("cookie %q must be a string", k)
		}
		out[k] = str
	}
	return out, nil
}

func parseCookieHeader(h string, into map[string]string) {
	for _, part := range strings.Split(h, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		k = strings.TrimSpace(k)
		if ok && k != "" {
			into[k] = strings.TrimSpace(v)
		}
	}
}

func parseCurlCookies(cmd string) (map[string]string, error) {
	if strings.Contains(cmd, `^"`) {
		return nil, errors.New("this looks like the Windows cmd format; copy the request as cURL (bash) instead")
	}
	toks, err := shellSplit(cmd)
	if err != nil {
		return nil, fmt.Errorf("could not parse cURL command: %w", err)
	}

	out := map[string]string{}
	found := false
	addHeader := func(h string) {
		if name, val, ok := strings.Cut(h, ":"); ok && strings.EqualFold(strings.TrimSpace(name), "cookie") {
			parseCookieHeader(val, out)
			found = true
		}
	}

	for i := 0; i < len(toks); i++ {
		switch t := toks[i]; {
		case t == "-H" || t == "--header":
			if i+1 < len(toks) {
				i++
				addHeader(toks[i])
			}
		case strings.HasPrefix(t, "--header="):
			addHeader(strings.TrimPrefix(t, "--header="))
		case t == "-b" || t == "--cookie":
			if i+1 < len(toks) {
				i++
				if strings.Contains(toks[i], "=") {
					parseCookieHeader(toks[i], out)
					found = true
				}
			}
		case strings.HasPrefix(t, "--cookie="):
			if v := strings.TrimPrefix(t, "--cookie="); strings.Contains(v, "=") {
				parseCookieHeader(v, out)
				found = true
			}
		}
	}
	if !found {
		return nil, errors.New("no cookies found in the cURL command")
	}
	return out, nil
}

func shellSplit(s string) ([]string, error) {
	var (
		toks []string
		cur  strings.Builder
		in   bool
	)
	flush := func() {
		if in {
			toks = append(toks, cur.String())
			cur.Reset()
			in = false
		}
	}

	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			flush()
			i++

		case c == '\\':
			switch {
			case strings.HasPrefix(s[i:], "\\\r\n"):
				i += 3
			case strings.HasPrefix(s[i:], "\\\n"):
				i += 2
			case i+1 < len(s):
				cur.WriteByte(s[i+1])
				in = true
				i += 2
			default:
				i++
			}

		case c == '\'':
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return nil, errors.New("unterminated single quote")
			}
			cur.WriteString(s[i+1 : i+1+end])
			in = true
			i += end + 2

		case c == '"':
			i++
			closed := false
			for i < len(s) {
				d := s[i]
				if d == '"' {
					closed = true
					i++
					break
				}
				if d == '\\' && i+1 < len(s) {
					switch s[i+1] {
					case '"', '\\', '$', '`':
						cur.WriteByte(s[i+1])
						i += 2
						continue
					case '\n':
						i += 2
						continue
					}
				}
				cur.WriteByte(d)
				i++
			}
			if !closed {
				return nil, errors.New("unterminated double quote")
			}
			in = true

		case c == '$' && i+1 < len(s) && s[i+1] == '\'':
			i += 2
			closed := false
			for i < len(s) {
				d := s[i]
				if d == '\'' {
					closed = true
					i++
					break
				}
				if d == '\\' && i+1 < len(s) {
					rep, w := ansiEscape(s[i+1:])
					cur.WriteString(rep)
					i += 1 + w
					continue
				}
				cur.WriteByte(d)
				i++
			}
			if !closed {
				return nil, errors.New("unterminated $'...' quote")
			}
			in = true

		default:
			cur.WriteByte(c)
			in = true
			i++
		}
	}
	flush()
	return toks, nil
}

func ansiEscape(r string) (string, int) {
	switch r[0] {
	case 'n':
		return "\n", 1
	case 'r':
		return "\r", 1
	case 't':
		return "\t", 1
	case '\\':
		return "\\", 1
	case '\'':
		return "'", 1
	case '"':
		return `"`, 1
	case 'x':
		if n, w := hexRun(r[1:], 2); w > 0 {
			return string([]byte{byte(n)}), 1 + w
		}
	case 'u':
		if n, w := hexRun(r[1:], 4); w > 0 {
			return string(rune(n)), 1 + w
		}
	case 'U':
		if n, w := hexRun(r[1:], 8); w > 0 {
			return string(rune(n)), 1 + w
		}
	}
	return "\\" + r[:1], 1
}

func hexRun(s string, limit int) (uint32, int) {
	var n uint32
	w := 0
	for w < len(s) && w < limit {
		d, ok := hexVal(s[w])
		if !ok {
			break
		}
		n = n<<4 | d
		w++
	}
	return n, w
}

func hexVal(c byte) (uint32, bool) {
	switch {
	case c >= '0' && c <= '9':
		return uint32(c - '0'), true
	case c >= 'a' && c <= 'f':
		return uint32(c-'a') + 10, true
	case c >= 'A' && c <= 'F':
		return uint32(c-'A') + 10, true
	}
	return 0, false
}
