package report

import (
    "context"
    "io"
    "net/http"
    "net/http/httptest"
    "strings"
    "testing"
    "time"
)

func TestPushRequiresHTTPS(t *testing.T) {
    _, err := NewClient("http://example.test", "k").Push(context.Background(), Payload{Version: "1"})
    if err == nil || !strings.Contains(err.Error(), "HTTPS") { t.Fatalf("got %v", err) }
}

func TestPushSendsIdempotencyInHeaderAndPayload(t *testing.T) {
    var header, payload string
    srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        header = r.Header.Get("Idempotency-Key")
        b, _ := io.ReadAll(r.Body)
        payload = string(b)
        w.WriteHeader(http.StatusAccepted)
        io.WriteString(w, `{"scan_id":"s","accepted":0,"rejected":[]}`)
    }))
    defer srv.Close()
    c := NewClient(srv.URL, "k"); c.HC = srv.Client()
    if _, err := c.Push(context.Background(), Payload{Version: "1"}); err != nil { t.Fatal(err) }
    if header == "" || !strings.Contains(payload, `"idempotency_key":"`+header+`"`) { t.Fatalf("header=%q payload=%s", header, payload) }
}

func TestPushRejectsInvalidAck(t *testing.T) {
    srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusAccepted); io.WriteString(w, `{}`) }))
    defer srv.Close()
    c := NewClient(srv.URL, "k"); c.HC = srv.Client()
    _, err := c.Push(context.Background(), Payload{Version: "1"})
    if err == nil || !strings.Contains(err.Error(), "ACK") { t.Fatalf("got %v", err) }
}

func TestPushRetriesOnlyRetryableResponsesWithInjectedBackoff(t *testing.T) {
    attempts := 0
    srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        attempts++
        if attempts < 3 { w.WriteHeader(http.StatusServiceUnavailable); return }
        w.WriteHeader(http.StatusAccepted); io.WriteString(w, `{"scan_id":"s","accepted":0,"rejected":[]}`)
    }))
    defer srv.Close()
    var waits []time.Duration
    c := NewClient(srv.URL, "k"); c.HC = srv.Client()
    c.Retry = RetryPolicy{MaxAttempts: 3, InitialBackoff: time.Second, Sleep: func(context.Context, time.Duration) error { waits = append(waits, time.Second); return nil }}
    if _, err := c.Push(context.Background(), Payload{Version: "1"}); err != nil { t.Fatal(err) }
    if attempts != 3 || len(waits) != 2 { t.Fatalf("attempts=%d waits=%v", attempts, waits) }
}

func TestPushDoesNotRetryClientError(t *testing.T) {
    attempts := 0
    srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { attempts++; w.WriteHeader(http.StatusBadRequest) }))
    defer srv.Close()
    c := NewClient(srv.URL, "k"); c.HC = srv.Client(); c.Retry = RetryPolicy{MaxAttempts: 5, Sleep: func(context.Context, time.Duration) error { t.Fatal("retried"); return nil }}
    if _, err := c.Push(context.Background(), Payload{Version: "1"}); err == nil || attempts != 1 { t.Fatalf("err=%v attempts=%d", err, attempts) }
}

func TestPushBoundsResponse(t *testing.T) {
    srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusAccepted); io.WriteString(w, strings.Repeat("x", maxAckBytes+1)) }))
    defer srv.Close()
    c := NewClient(srv.URL, "k"); c.HC = srv.Client()
    _, err := c.Push(context.Background(), Payload{Version: "1"})
    if err == nil || !strings.Contains(err.Error(), "too large") { t.Fatalf("got %v", err) }
}
