// Package order generates the sort keys that put sibling nodes in document
// order (SPEC.md Appendix C).
//
// Keys are compared byte by byte, never by a database collation, so that every
// driver agrees on order (R-STORE-10). They are not part of ScreenJSON: they
// live in the server envelope (R-STORE-08).
package order

import (
	"fmt"
	"strings"
)

// Alphabet is the 62 characters a key is built from, in byte order. Because the
// alphabet is in byte order, comparing two keys as Go strings compares them as
// sequences of digits in base 62.
const Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// base is the number of digits in the alphabet.
const base = len(Alphabet)

// MaxLen is the length past which the write queue re-keys a sibling list
// instead of letting keys grow without bound (R-STORE-11).
const MaxLen = 64

// digit maps a byte of the alphabet to its value. Index by byte; -1 means the
// byte is not a valid key character.
var digit = func() [256]int {
	var t [256]int
	for i := range t {
		t[i] = -1
	}
	for i := 0; i < base; i++ {
		t[Alphabet[i]] = i
	}
	return t
}()

// Valid reports whether s is a well-formed order key: non-empty, built only
// from the alphabet, and not ending in '0'.
//
// The trailing-'0' rule is what makes Between total. A key ending in '0' has no
// room below it: any key that is a strict prefix-extension downward would need a
// digit below zero. Forbidding it means every key has a gap on both sides.
func Valid(s string) bool {
	if s == "" || s[len(s)-1] == '0' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if digit[s[i]] < 0 {
			return false
		}
	}
	return true
}

// digitAt returns the value of the i'th digit of key k, using the supplied
// value for positions past the end of k.
func digitAt(k string, i, beyond int) int {
	if i < len(k) {
		return digit[k[i]]
	}
	return beyond
}

// Between returns a key strictly between a and b.
//
// An empty a means "before everything" and an empty b means "after everything",
// so Between("", "") produces the first key of an empty list. It is an error to
// call it with a >= b when both are set.
//
// The walk compares a and b digit by digit, treating a missing digit of a as 0
// (a behaves like a followed by endless zeros) and a missing digit of b as base
// (b behaves like the very top of the range):
//
//   - where the digits differ by more than one there is room between them, so
//     take the midpoint and stop;
//   - where they differ by exactly one or not at all there is no room at this
//     position, so copy a's digit and carry on one digit deeper. Once a digit of
//     a has been copied that is below b's, b can no longer constrain the digits
//     after it, so from that point b is treated as "end".
//
// The result never ends in '0' because the loop only stops on a midpoint that
// is strictly above the digit below it, which for the final position is at
// least 1.
func Between(a, b string) (string, error) {
	if a != "" && !Valid(a) {
		return "", fmt.Errorf("order: left key %q is not a valid key", a)
	}
	if b != "" && !Valid(b) {
		return "", fmt.Errorf("order: right key %q is not a valid key", b)
	}
	if a != "" && b != "" && a >= b {
		return "", fmt.Errorf("order: left key %q is not below right key %q", a, b)
	}

	var out strings.Builder
	// unbounded records that b has stopped constraining the result, which
	// happens as soon as we emit a digit strictly below b's at that position.
	unbounded := b == ""

	for i := 0; ; i++ {
		lo := digitAt(a, i, 0)
		hi := base
		if !unbounded {
			hi = digitAt(b, i, base)
		}

		if hi-lo > 1 {
			// There is a free digit between them. Take the middle so that later
			// inserts on either side have room.
			out.WriteByte(Alphabet[(lo+hi)/2])
			return out.String(), nil
		}

		// No room here: keep a's digit and look one digit deeper. If that digit
		// is strictly below b's, everything after it is free.
		out.WriteByte(Alphabet[lo])
		if !unbounded && lo < hi {
			unbounded = true
		}
	}
}

// Spread returns n keys of equal length, evenly spaced across the range, for
// importing a list whose order is already known (R-STORE-08).
//
// Even spacing leaves room on both sides of every key, so a later insert
// anywhere in the list needs one short key rather than a long one.
func Spread(n int) ([]string, error) {
	if n < 0 {
		return nil, fmt.Errorf("order: cannot spread %d keys", n)
	}
	if n == 0 {
		return nil, nil
	}

	// Pick the shortest width whose value space gives a step of at least 2:
	// base**width >= 2*(n+1). A step of 1 would leave no room to nudge a key
	// off a trailing zero without colliding with the next one, and no room for
	// a later insert without lengthening a key.
	//
	// Width is capped at 10 because base**11 overflows a 64-bit int, and
	// base**10 is about 8.4e17 slots — far past any real sibling list.
	const maxWidth = 10
	want := 2 * (n + 1)
	width, capacity := 1, base
	for capacity < want {
		if width == maxWidth {
			return nil, fmt.Errorf("order: cannot spread %d keys", n)
		}
		width++
		capacity *= base
	}

	// step >= 2 by the choice of width above.
	step := capacity / (n + 1)
	keys := make([]string, n)
	for i := range keys {
		v := step * (i + 1)
		k := encode(v, width)
		// Even spacing can land on a value ending in '0', which is not a legal
		// key. Nudging up by one keeps the key inside its own slot, because
		// step is at least 2 whenever a trailing zero is possible.
		if k[len(k)-1] == '0' {
			k = encode(v+1, width)
		}
		keys[i] = k
	}
	return keys, nil
}

// encode renders v as a base-62 string of exactly width digits.
func encode(v, width int) string {
	buf := make([]byte, width)
	for i := width - 1; i >= 0; i-- {
		buf[i] = Alphabet[v%base]
		v /= base
	}
	return string(buf)
}

// NeedsRekey reports whether a sibling list should be re-keyed because one of
// its keys has grown past MaxLen (R-STORE-11).
func NeedsRekey(keys []string) bool {
	for _, k := range keys {
		if len(k) > MaxLen {
			return true
		}
	}
	return false
}
