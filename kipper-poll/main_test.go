package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWaitStopsWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	if wait(ctx, time.Hour) {
		t.Fatal("wait must report false once the context is done")
	}
	if time.Since(start) > time.Second {
		t.Fatal("wait must return as soon as the context is done")
	}
}

func TestWaitReturnsTrueAfterTheDelay(t *testing.T) {
	if !wait(context.Background(), time.Millisecond) {
		t.Fatal("wait must report true when the delay passes")
	}
}

func TestServeMinIOStopsWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveMinIO(ctx, "127.0.0.1:0", "http://127.0.0.1:1") }()

	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveMinIO: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the webhook server must stop once the context ends")
	}
}

func TestSendEventGivesUpWhenTheContextEnds(t *testing.T) {
	hang := make(chan struct{})
	fn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-hang:
		case <-r.Context().Done():
		}
	}))
	defer fn.Close()
	defer close(hang)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := sendEvent(ctx, fn.URL, map[string]string{"k": "v"}); err == nil {
		t.Fatal("delivery to a function that never answers must fail once the context ends")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("delivery must stop as soon as the context ends")
	}
}

func TestServeMinIOClosesRequestsThatOutliveTheGracePeriod(t *testing.T) {
	webhookShutdownGrace = 200 * time.Millisecond
	defer func() { webhookShutdownGrace = 10 * time.Second }()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveMinIO(ctx, addr, "http://127.0.0.1:1") }()
	time.Sleep(100 * time.Millisecond)

	body, stall := io.Pipe()
	defer func() { _ = stall.Close() }()
	go func() {
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/", body)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	_, _ = stall.Write([]byte("{"))
	time.Sleep(100 * time.Millisecond)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a request outliving the grace period must be closed, not reported as a failure: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the webhook server must stop after its grace period")
	}
}
