package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestServeDrainsInFlightRequest(t *testing.T) {
	t.Parallel()
	lc := net.ListenConfig{}
	listener, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := make(chan struct{})
	proceed := make(chan struct{})
	requestDone := make(chan error, 1)
	serverDone := make(chan error, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-proceed:
			w.WriteHeader(http.StatusNoContent)
		case <-r.Context().Done():
			t.Error("in-flight request canceled before graceful drain")
		}
	})
	go func() {
		serverDone <- serve(
			ctx,
			listener,
			handler,
			func(ctx context.Context) error { <-ctx.Done(); return nil },
		)
	}()
	client := &http.Client{Timeout: 3 * time.Second}
	go func() {
		resp, err := client.Get("http://" + listener.Addr().String())
		if err != nil {
			requestDone <- err
			return
		}
		_, readErr := io.Copy(io.Discard, resp.Body)
		closeErr := resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			requestDone <- errors.New("request did not complete normally")
			return
		}
		requestDone <- errors.Join(readErr, closeErr)
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		close(proceed)
		t.Fatal("request never entered handler")
	}
	cancel()
	close(proceed)
	for _, done := range []<-chan error{requestDone, serverDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("graceful shutdown did not finish")
		}
	}
}

func TestServeListenerFailureStopsWorker(t *testing.T) {
	t.Parallel()
	lc := net.ListenConfig{}
	listener, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	err = serve(
		t.Context(),
		listener,
		http.NotFoundHandler(),
		func(ctx context.Context) error { <-ctx.Done(); return nil },
	)
	if err == nil {
		t.Fatal("listener failure was not reported")
	}
}

func TestServeCancellationJoinsWorker(t *testing.T) {
	t.Parallel()
	lc := net.ListenConfig{}
	listener, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started := make(chan struct{})
	stopped := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- serve(
			ctx,
			listener,
			http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }),
			func(ctx context.Context) error {
				close(started)
				<-ctx.Done()
				close(stopped)
				return nil
			},
		)
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("worker never started")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not complete")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("worker was not joined")
	}
}

func TestServeWorkerFailureStopsServer(t *testing.T) {
	t.Parallel()
	lc := net.ListenConfig{}
	listener, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	err = serve(
		t.Context(),
		listener,
		http.NotFoundHandler(),
		func(context.Context) error { return errors.New("synthetic failure") },
	)
	if err == nil {
		t.Fatal("worker failure must fail the process")
	}
}
