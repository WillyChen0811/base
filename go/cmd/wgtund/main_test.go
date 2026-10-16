package main

import (
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testGrace = 50 * time.Millisecond

func runWatch(done, eof, exited chan struct{}, calls *int32, code *int32) chan struct{} {
	fin := make(chan struct{})
	go func() {
		watchShutdown(done, eof, exited, testGrace, func(c int) {
			atomic.AddInt32(calls, 1)
			atomic.StoreInt32(code, int32(c))
		})
		close(fin)
	}()
	return fin
}

func TestWatchShutdownForcesExitOnDone(t *testing.T) {
	done, eof, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls, code int32
	fin := runWatch(done, eof, exited, &calls, &code)
	close(done)
	<-fin
	time.Sleep(2 * testGrace)
	if atomic.LoadInt32(&calls) != 1 || atomic.LoadInt32(&code) != 0 {
		t.Fatalf("exit calls=%d code=%d, want 1/0", calls, code)
	}
}

func TestWatchShutdownForcesExitOnEOF(t *testing.T) {
	done, eof, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls, code int32
	fin := runWatch(done, eof, exited, &calls, &code)
	close(eof)
	<-fin
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("exit calls=%d, want 1", calls)
	}
}

func TestWatchShutdownNoExitWhenExitedInTime(t *testing.T) {
	done, eof, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls, code int32
	fin := runWatch(done, eof, exited, &calls, &code)
	close(done)
	time.Sleep(testGrace / 5)
	close(exited)
	<-fin
	time.Sleep(2 * testGrace)
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("exit called %d times, want 0", calls)
	}
}

func TestWatchShutdownNoEventExitedReturns(t *testing.T) {
	done, eof, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls, code int32
	fin := runWatch(done, eof, exited, &calls, &code)
	close(exited)
	<-fin
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatal("exit should not be called")
	}
}

type closeRecorder struct {
	io.Reader
	closed bool
}

func (c *closeRecorder) Close() error { c.closed = true; return nil }

func TestEOFReader(t *testing.T) {
	var n int32
	rec := &closeRecorder{Reader: strings.NewReader("ab")}
	r := newEOFReader(rec, func() { atomic.AddInt32(&n, 1) })
	if _, err := io.Copy(io.Discard, r); err != nil {
		t.Fatal(err)
	}
	r.Read(make([]byte, 1))
	if n != 1 {
		t.Fatalf("onEOF called %d times, want 1", n)
	}
	var c io.Closer = r
	c.Close()
	if !rec.closed {
		t.Fatal("Close not forwarded")
	}
}
