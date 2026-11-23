package gen

import (
	"go/token"
	"strings"
	"unicode"
)

// goCamelCase converts a proto field name to the Go field name that
// protoc-gen-go generates (same algorithm as protobuf's internal strs package).
func goCamelCase(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '.' && i+1 < len(s) && isLower(s[i+1]):
		case c == '.':
			b = append(b, '_')
		case c == '_' && (i == 0 || s[i-1] == '.'):
			b = append(b, 'X')
		case c == '_' && i+1 < len(s) && isLower(s[i+1]):
		case isDigit(c):
			b = append(b, c)
		default:
			if isLower(c) {
				c -= 'a' - 'A'
			}
			b = append(b, c)
			for ; i+1 < len(s) && isLower(s[i+1]); i++ {
				b = append(b, s[i+1])
			}
		}
	}
	return string(b)
}

func isLower(c byte) bool { return 'a' <= c && c <= 'z' }
func isDigit(c byte) bool { return '0' <= c && c <= '9' }

// snake converts PascalCase to lower_snake_case ("HTTPRoute" -> "http_route").
func snake(s string) string {
	r := []rune(s)
	var b strings.Builder
	for i, c := range r {
		if unicode.IsUpper(c) {
			prevLower := i > 0 && (unicode.IsLower(r[i-1]) || unicode.IsDigit(r[i-1]))
			nextLower := i > 0 && i+1 < len(r) && unicode.IsLower(r[i+1]) && unicode.IsUpper(r[i-1])
			if prevLower || nextLower {
				b.WriteByte('_')
			}
			b.WriteRune(unicode.ToLower(c))
			continue
		}
		b.WriteRune(c)
	}
	return b.String()
}

// pascal converts "order-service" or "order_service" to "OrderService".
func pascal(s string) string {
	return goCamelCase(strings.ReplaceAll(s, "-", "_"))
}

// lowerFirst lower-cases the leading rune.
func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToLower(r[0])
	return string(r)
}

// words turns PascalCase into lower-case words ("OrderLine" -> "order line").
func words(s string) string {
	return strings.ReplaceAll(snake(s), "_", " ")
}

// reservedIdents are names that generated code already uses in scope, so
// entity variables must not shadow them.
var reservedIdents = map[string]bool{
	"m": true, "s": true, "c": true, "x": true, "t": true, "v": true, "p": true,
	"pb": true, "ctx": true, "id": true, "err": true, "in": true, "req": true,
	"cur": true, "now": true, "out": true, "next": true, "created": true,
	"updated": true, "items": true, "total": true, "store": true, "proto": true,
	"status": true, "codes": true, "got": true, "resp": true, "st": true,
}

// safeVar returns a Go identifier for a local variable holding an entity.
func safeVar(name string) string {
	v := lowerFirst(goCamelCase(snake(name)))
	if token.IsKeyword(v) || reservedIdents[v] || isPredeclared(v) {
		v += "Item"
	}
	return v
}

func isPredeclared(s string) bool {
	switch s {
	case "any", "bool", "byte", "comparable", "complex64", "complex128", "error", "float32", "float64",
		"int", "int8", "int16", "int32", "int64", "rune", "string", "uint", "uint8", "uint16", "uint32",
		"uint64", "uintptr", "true", "false", "iota", "nil", "append", "cap", "clear", "close", "complex",
		"copy", "delete", "imag", "len", "make", "max", "min", "new", "panic", "print", "println", "real",
		"recover":
		return true
	}
	return false
}
