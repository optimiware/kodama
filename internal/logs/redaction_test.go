package logs

import (
	"bytes"
	"io"
	"math/rand"
	"strings"
	"testing"
	"testing/iotest"
)

func redactAll(t *testing.T, values [][]byte, chunks ...string) string {
	t.Helper()
	r, err := NewRedactor(values)
	if err != nil {
		t.Fatal(err)
	}
	var out []byte
	for _, c := range chunks {
		out = append(out, r.Write([]byte(c))...)
	}
	out = append(out, r.Flush()...)
	return string(out)
}

// TestRedactionBoundaries: no known value survives any split of the stream,
// overlapping values merge, EOF releases held bytes, binary input passes and the
// buffer stays bounded.
func TestRedactionBoundaries(t *testing.T) {
	secret := []byte("hunter2-token")
	input := "start " + string(secret) + " middle " + string(secret) + " end"
	want := "start " + Marker + " middle " + Marker + " end"

	t.Run("every split point", func(t *testing.T) {
		for i := 0; i <= len(input); i++ {
			if got := redactAll(t, [][]byte{secret}, input[:i], input[i:]); got != want {
				t.Fatalf("split at %d: %q", i, got)
			}
		}
	})
	t.Run("every pair of split points", func(t *testing.T) {
		for i := 0; i <= len(input); i += 3 {
			for j := i; j <= len(input); j += 2 {
				if got := redactAll(t, [][]byte{secret}, input[:i], input[i:j], input[j:]); got != want {
					t.Fatalf("split at %d,%d: %q", i, j, got)
				}
			}
		}
	})
	t.Run("byte at a time", func(t *testing.T) {
		var chunks []string
		for i := range input {
			chunks = append(chunks, input[i:i+1])
		}
		if got := redactAll(t, [][]byte{secret}, chunks...); got != want {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("overlapping values leave no remnant", func(t *testing.T) {
		vals := [][]byte{[]byte("abcd"), []byte("cdef"), []byte("bc")}
		for i := 0; i <= len("xxabcdefyy"); i++ {
			got := redactAll(t, vals, "xxabcdefyy"[:i], "xxabcdefyy"[i:])
			if got != "xx"+Marker+"yy" {
				t.Fatalf("split %d: %q", i, got)
			}
		}
	})
	t.Run("prefix of a value at EOF is released unchanged", func(t *testing.T) {
		if got := redactAll(t, [][]byte{secret}, "tail hunter2-"); got != "tail hunter2-" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("longest value wins at one position", func(t *testing.T) {
		got := redactAll(t, [][]byte{[]byte("abc"), []byte("abcdef")}, "..abcdef..")
		if got != ".."+Marker+".." {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("binary input", func(t *testing.T) {
		bin := []byte{0, 1, 2, 0xff, 0xfe, 0x00}
		in := append(append([]byte{0xc3, 0x28}, bin...), 'x')
		got := redactAll(t, [][]byte{bin}, string(in[:4]), string(in[4:]))
		if got != string([]byte{0xc3, 0x28})+Marker+"x" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("random splits never leak", func(t *testing.T) {
		rng := rand.New(rand.NewSource(7))
		vals := [][]byte{[]byte("alpha-secret"), []byte("secret-beta"), []byte("zz")}
		base := strings.Repeat("noise alpha-secret-beta zz secret-beta alpha-secret ", 20)
		for n := 0; n < 200; n++ {
			var chunks []string
			for rest := base; len(rest) > 0; {
				k := 1 + rng.Intn(17)
				if k > len(rest) {
					k = len(rest)
				}
				chunks = append(chunks, rest[:k])
				rest = rest[k:]
			}
			got := redactAll(t, vals, chunks...)
			for _, v := range vals {
				if strings.Contains(got, string(v)) {
					t.Fatalf("leaked %q in %q", v, got)
				}
			}
		}
	})
	t.Run("no values passes through", func(t *testing.T) {
		if got := redactAll(t, nil, "abc", "def"); got != "abcdef" {
			t.Fatalf("got %q", got)
		}
	})
}

func TestRedactorBounds(t *testing.T) {
	if _, err := NewRedactor([][]byte{{}}); err == nil {
		t.Fatal("an empty value would match everywhere and must be refused")
	}
	if _, err := NewRedactor([][]byte{bytes.Repeat([]byte("a"), MaxRedactValueLen+1)}); err == nil {
		t.Fatal("an oversized value must be refused")
	}
	many := make([][]byte, MaxRedactValues+1)
	for i := range many {
		many[i] = []byte{byte(i), 'x'}
	}
	if _, err := NewRedactor(many); err == nil {
		t.Fatal("an oversized set must be refused")
	}
	// A hostile stream cannot grow the held buffer without bound: every byte
	// starts a long partial match.
	r, err := NewRedactor([][]byte{bytes.Repeat([]byte("a"), 64), []byte("b")})
	if err != nil {
		t.Fatal(err)
	}
	var emitted int
	for i := 0; i < 20000; i++ {
		emitted += len(r.Write([]byte("a")))
		if len(r.buf) > r.holdCap+1 {
			t.Fatalf("buffer grew to %d (cap %d)", len(r.buf), r.holdCap)
		}
	}
	if emitted == 0 {
		t.Fatal("bytes that cannot complete a value must be released")
	}
}

func TestRedactingReaderErrorsDropHeldBytes(t *testing.T) {
	r, _ := NewRedactor([][]byte{[]byte("hunter2")})
	src := io.MultiReader(strings.NewReader("ok hunt"), iotest.ErrReader(io.ErrUnexpectedEOF))
	out, err := io.ReadAll(newRedactingReader(src, r))
	if err != io.ErrUnexpectedEOF {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(string(out), "hunt") || string(out) != "ok " {
		t.Fatalf("held bytes of a failed stream must not be released: %q", out)
	}
	r2, _ := NewRedactor([][]byte{[]byte("hunter2")})
	out, err = io.ReadAll(iotest.OneByteReader(newRedactingReader(strings.NewReader("a hunter2 b"), r2)))
	if err != nil || string(out) != "a "+Marker+" b" {
		t.Fatalf("got %q, %v", out, err)
	}
}

// TestRedactionIsSplitInvariant: redaction output must not depend on where the
// stream is cut. The unsplit result is the reference; every two-way split, a
// byte-at-a-time stream and random splits must equal it exactly, so a leaked
// suffix or prefix of any value (not only the whole value) fails. Value sets
// include values that are prefixes, suffixes or overlaps of one another.
func TestRedactionIsSplitInvariant(t *testing.T) {
	sets := map[string][][]byte{
		"prefix-nested":    {[]byte("abc"), []byte("abcdef")},
		"prefix-chain":     {[]byte("ab"), []byte("abcd"), []byte("abcdefgh")},
		"secret-vs-longer": {[]byte("secret"), []byte("secret123")},
		"overlap":          {[]byte("abcd"), []byte("cdef"), []byte("bc")},
		"suffix-nested":    {[]byte("def"), []byte("abcdef")},
	}
	inputs := []string{
		"..abcdef..", "abcdefgh", "xabcdefghx abcd", "secret123 secret secret12", "tail secret12",
		"abcabcdefabc", "zzabcdefzz", "abcde", "ab", "abcdefg",
	}
	for name, vals := range sets {
		for _, in := range inputs {
			want := redactAll(t, vals, in)
			for _, v := range vals {
				if strings.Contains(want, string(v)) {
					t.Fatalf("%s: reference leaked %q in %q", name, v, want)
				}
			}
			for i := 0; i <= len(in); i++ {
				if got := redactAll(t, vals, in[:i], in[i:]); got != want {
					t.Fatalf("%s: %q split at %d: got %q, want %q", name, in, i, got, want)
				}
			}
			bytewise := make([]string, 0, len(in))
			for i := 0; i < len(in); i++ {
				bytewise = append(bytewise, in[i:i+1])
			}
			if got := redactAll(t, vals, bytewise...); got != want {
				t.Fatalf("%s: %q byte-at-a-time: got %q, want %q", name, in, got, want)
			}
			rng := rand.New(rand.NewSource(int64(len(in)) + 11))
			for n := 0; n < 50; n++ {
				var chunks []string
				for rest := in; len(rest) > 0; {
					k := 1 + rng.Intn(4)
					if k > len(rest) {
						k = len(rest)
					}
					chunks = append(chunks, rest[:k])
					rest = rest[k:]
				}
				if got := redactAll(t, vals, chunks...); got != want {
					t.Fatalf("%s: %q chunks %q: got %q, want %q", name, in, chunks, got, want)
				}
			}
		}
	}
}

// TestRedactionRegression pins the reported leak: the shorter value matched
// before the longer, still incomplete one, so its tail was released.
func TestRedactionRegression(t *testing.T) {
	got := redactAll(t, [][]byte{[]byte("abc"), []byte("abcdef")}, "abc", "def")
	if got != Marker {
		t.Fatalf("got %q, want %q: the tail of a longer value leaked", got, Marker)
	}
}
