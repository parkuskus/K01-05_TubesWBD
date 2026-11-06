package telemetry

import (
	"context"
	"errors"
	"strings"
	"unicode"
)

func safeError(err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if code := SQLState(err); code != "" {
		return errors.New("database operation failed (SQLSTATE " + code + ")")
	}
	return errors.New("operation failed")
}

func SQLState(err error) string {
	var state interface{ SQLState() string }
	if errors.As(err, &state) {
		code := state.SQLState()
		if len(code) == 5 && strings.IndexFunc(code, func(r rune) bool {
			return !(r >= '0' && r <= '9' || r >= 'A' && r <= 'Z')
		}) == -1 {
			return code
		}
	}
	return ""
}

func RedactSQL(query string) string {
	var out strings.Builder
	for i := 0; i < len(query); {
		switch {
		case strings.HasPrefix(query[i:], "--"):
			end := strings.IndexByte(query[i:], '\n')
			if end < 0 {
				i = len(query)
			} else {
				i += end + 1
			}
			out.WriteByte(' ')
		case strings.HasPrefix(query[i:], "/*"):
			i += 2
			depth := 1
			for i < len(query) && depth > 0 {
				if strings.HasPrefix(query[i:], "/*") {
					depth++
					i += 2
				} else if strings.HasPrefix(query[i:], "*/") {
					depth--
					i += 2
				} else {
					i++
				}
			}
			out.WriteByte(' ')
		case query[i] == '\'' || query[i] == '"':
			quote := query[i]
			i++
			for i < len(query) {
				if query[i] == '\\' {
					return redacted
				} else if query[i] == quote {
					i++
					if i < len(query) && query[i] == quote {
						i++
						continue
					}
					break
				} else {
					i++
				}
			}
			out.WriteByte('?')
		case query[i] == '$':
			end := i + 1
			for end < len(query) && (query[end] == '_' || query[end] >= 'a' && query[end] <= 'z' || query[end] >= 'A' && query[end] <= 'Z' || query[end] >= '0' && query[end] <= '9') {
				end++
			}
			if end < len(query) && query[end] == '$' {
				delimiter := query[i : end+1]
				closing := strings.Index(query[end+1:], delimiter)
				if closing < 0 {
					i = len(query)
				} else {
					i = end + 1 + closing + len(delimiter)
				}
				out.WriteByte('?')
			} else if end > i+1 && query[i+1] >= '0' && query[i+1] <= '9' {
				out.WriteString(query[i:end])
				i = end
			} else {
				return redacted
			}
		case query[i] >= '0' && query[i] <= '9':
			for i < len(query) && (query[i] >= '0' && query[i] <= '9' || query[i] == '.' || query[i] >= 'a' && query[i] <= 'z' || query[i] >= 'A' && query[i] <= 'Z' || query[i] == '_') {
				i++
			}
			out.WriteByte('?')
		case query[i] == '_' || query[i] >= 'a' && query[i] <= 'z' || query[i] >= 'A' && query[i] <= 'Z':
			start := i
			for i < len(query) && (query[i] == '_' || query[i] >= 'a' && query[i] <= 'z' || query[i] >= 'A' && query[i] <= 'Z' || query[i] >= '0' && query[i] <= '9') {
				i++
			}
			out.WriteString(query[start:i])
		case unicode.IsSpace(rune(query[i])) || strings.ContainsRune("?(),.;:+-*/%<>=!|&[]", rune(query[i])):
			out.WriteByte(query[i])
			i++
		default:
			return redacted
		}
	}
	clean := strings.Join(strings.Fields(out.String()), " ")
	if len(clean) > 4096 {
		clean = clean[:4096] + " [TRUNCATED]"
	}
	return clean
}
