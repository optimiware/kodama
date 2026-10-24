package logs

import (
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func TestLineReaderBounds(t *testing.T) {
	t.Run("lines, CRLF and a final unterminated line", func(t *testing.T) {
		lr := newLineReader(strings.NewReader("a\r\nbb\nccc"), 100, 1000)
		var got []string
		for {
			l, cut, err := lr.next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil || cut {
				t.Fatalf("%v %v", err, cut)
			}
			got = append(got, string(l))
		}
		if strings.Join(got, "|") != "a|bb|ccc" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("an oversized line is cut and the rest discarded", func(t *testing.T) {
		src := strings.Repeat("x", 100000) + "\nnext\n"
		lr := newLineReader(iotest.OneByteReader(strings.NewReader(src)), 10, 1<<20)
		l, cut, err := lr.next()
		if err != nil || !cut || string(l) != strings.Repeat("x", 10) {
			t.Fatalf("%q %v %v", l, cut, err)
		}
		l, cut, err = lr.next()
		if err != nil || cut || string(l) != "next" {
			t.Fatalf("%q %v %v", l, cut, err)
		}
	})
	t.Run("the scan budget bounds consumption of an endless source", func(t *testing.T) {
		endless := &countingReader{b: []byte(strings.Repeat("e", 4096))}
		lr := newLineReader(endless, 64, 10000)
		var err error
		for i := 0; i < 1000 && err == nil; i++ {
			_, _, err = lr.next()
		}
		if !errors.Is(err, errScanLimit) {
			t.Fatalf("err = %v", err)
		}
		// bufio read-ahead may overshoot by one buffer, never unboundedly.
		if endless.n > 10000+8192 {
			t.Fatalf("consumed %d bytes of an endless source under a 10000 byte budget", endless.n)
		}
	})
	t.Run("a read error is not an end of file", func(t *testing.T) {
		lr := newLineReader(io.MultiReader(strings.NewReader("ok\npartial"), iotest.ErrReader(io.ErrUnexpectedEOF)), 64, 1000)
		if l, _, err := lr.next(); err != nil || string(l) != "ok" {
			t.Fatalf("%q %v", l, err)
		}
		if _, _, err := lr.next(); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("err = %v", err)
		}
	})
}

type countingReader struct {
	b []byte
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n := copy(p, c.b)
	c.n += n
	return n, nil
}

func TestSanitize(t *testing.T) {
	if got := sanitize([]byte("héllo")); got != "héllo" {
		t.Fatal(got)
	}
	if got := sanitize([]byte{'a', 0xff, 0xfe, 'b'}); got != "a\uFFFD\uFFFDb" && got != "a\uFFFDb" {
		t.Fatalf("%q", got)
	}
}

func TestWindowKeepsNewestWithinBudget(t *testing.T) {
	w := &window{n: 3, maxBytes: 20}
	for _, l := range []string{"one", "two", "three", "four", "five"} {
		w.push(l, false)
	}
	var got []string
	for _, l := range w.lines {
		got = append(got, l.text)
	}
	if strings.Join(got, ",") != "three,four,five" || w.evicted {
		t.Fatalf("lines = %v evicted=%v", got, w.evicted)
	}
	w = &window{n: 10, maxBytes: 9}
	for _, l := range []string{"aaaa", "bbbb", "cccc"} {
		w.push(l, false)
	}
	if len(w.lines) != 2 || w.lines[1].text != "cccc" || !w.evicted || w.bytes > 9 {
		t.Fatalf("%+v", w)
	}
}

func TestLimitsValidation(t *testing.T) {
	ok := Limits{MaxLines: 10, MaxBytes: 1000, MaxDuration: 1e9, MaxStreams: 1, WriteTimeout: 1e9}
	got, err := ok.normalized()
	if err != nil || got.DefaultTail != 10 || got.MaxLineBytes != 1000 {
		t.Fatalf("%+v %v", got, err)
	}
	for name, mut := range map[string]func(*Limits){
		"no lines":         func(l *Limits) { l.MaxLines = 0 },
		"no bytes":         func(l *Limits) { l.MaxBytes = 0 },
		"no duration":      func(l *Limits) { l.MaxDuration = 0 },
		"no streams":       func(l *Limits) { l.MaxStreams = 0 },
		"no write timeout": func(l *Limits) { l.WriteTimeout = 0 },
		"tail above lines": func(l *Limits) { l.MaxTail = 11 },
		"line above bytes": func(l *Limits) { l.MaxLineBytes = 2000 },
	} {
		l := ok
		mut(&l)
		if _, err := l.normalized(); !errors.Is(err, ErrNotConfigured) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestServiceRequiresItsPorts(t *testing.T) {
	if _, err := NewService(Config{}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("%v", err)
	}
	if _, err := NewController(ControllerConfig{}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("%v", err)
	}
}
