package sip

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/sip/pkg/config"
)

func newTestAdmission(url string) *admissionClient {
	return newAdmissionClient(&config.AdmissionConfig{
		URL: url, APIKey: "k", DefaultTimeout: time.Second, DefaultFallbackCode: 503,
	})
}

func TestAdmissionClientNil(t *testing.T) {
	require.Nil(t, newAdmissionClient(nil))
	require.Nil(t, newAdmissionClient(&config.AdmissionConfig{}))
}

func TestAdmissionDecideSendsAndParses(t *testing.T) {
	var gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Write([]byte(`{"decision_id":"d1","action":"reject","sip_code":486,"reason":"Busy","target":"t","call_bot_id":"b","fallback":{"timeout_ms":300,"sip_code":480,"reason":"Gone"}}`))
	}))
	defer srv.Close()
	a := newTestAdmission(srv.URL)
	d, err := a.Decide(context.Background(), AdmissionRequest{
		CallID: "c", SIPCallID: "s", From: "f", To: "t1", SourceIP: "1.2.3.4", TrunkID: "tr",
		Headers: map[string]string{"X-A": "b"},
	})
	require.NoError(t, err)
	require.Equal(t, "Bearer k", gotAuth)
	require.Equal(t, "c", gotBody["call_id"])
	require.Equal(t, "s", gotBody["sip_call_id"])
	require.Equal(t, "1.2.3.4", gotBody["source_ip"])
	require.Equal(t, "tr", gotBody["trunk_id"])
	require.Equal(t, map[string]any{"X-A": "b"}, gotBody["headers"])
	require.Equal(t, "reject", d.Action)
	require.Equal(t, 486, d.SIPCode)
	require.Equal(t, "d1", *d.DecisionID)
	require.Equal(t, "b", d.CallBotID)
	require.Equal(t, 300, d.Fallback.TimeoutMs)

	code, reason := a.FallbackFor("t1")
	require.Equal(t, 480, code)
	require.Equal(t, "Gone", reason)
}

func TestAdmissionDecideActions(t *testing.T) {
	for _, act := range []string{"accept", "redirect", "refer"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"decision_id":null,"action":"` + act + `","target":"sip:x@y","fallback":{"timeout_ms":1000,"sip_code":503,"reason":"x"}}`))
		}))
		d, err := newTestAdmission(srv.URL).Decide(context.Background(), AdmissionRequest{To: "n"})
		srv.Close()
		require.NoError(t, err)
		require.Equal(t, act, d.Action)
		require.Nil(t, d.DecisionID)
	}
}

func TestAdmissionDecideNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	_, err := newTestAdmission(srv.URL).Decide(context.Background(), AdmissionRequest{To: "n"})
	require.Error(t, err)
}

func TestAdmissionDecideTimeoutAndCachedTimeout(t *testing.T) {
	var delay atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Duration(delay.Load()))
		w.Write([]byte(`{"action":"accept","fallback":{"timeout_ms":300,"sip_code":480,"reason":"x"}}`))
	}))
	defer srv.Close()
	a := newAdmissionClient(&config.AdmissionConfig{URL: srv.URL, DefaultTimeout: 200 * time.Millisecond, DefaultFallbackCode: 503})

	delay.Store(int64(600 * time.Millisecond))
	_, err := a.Decide(context.Background(), AdmissionRequest{To: "n"})
	require.Error(t, err)

	delay.Store(0)
	_, err = a.Decide(context.Background(), AdmissionRequest{To: "n"})
	require.NoError(t, err)

	// cached timeout is now 300ms: a 250ms response passes, a 500ms one fails
	delay.Store(int64(250 * time.Millisecond))
	_, err = a.Decide(context.Background(), AdmissionRequest{To: "n"})
	require.NoError(t, err)
	delay.Store(int64(500 * time.Millisecond))
	_, err = a.Decide(context.Background(), AdmissionRequest{To: "n"})
	require.Error(t, err)
}

func TestAdmissionTimeoutClamp(t *testing.T) {
	a := newAdmissionClient(&config.AdmissionConfig{URL: "http://x", DefaultTimeout: time.Millisecond, DefaultFallbackCode: 503})
	require.Equal(t, 200*time.Millisecond, a.timeoutFor("n"))
	a.setFallback("n", 60000, 503, "x")
	require.Equal(t, 5*time.Second, a.timeoutFor("n"))
}

func TestAdmissionFallbackDefaults(t *testing.T) {
	a := newTestAdmission("http://x")
	code, reason := a.FallbackFor("unknown")
	require.Equal(t, 503, code)
	require.Equal(t, "Service Unavailable", reason)
	a.conf.DefaultFallbackCode = 486
	code, reason = a.FallbackFor("unknown")
	require.Equal(t, 486, code)
	require.Equal(t, sipStatus(486), reason)
}

func TestAdmissionFallbackSanitized(t *testing.T) {
	a := newTestAdmission("http://x")
	a.setFallback("n", 500, 480, "a\r\nb")
	_, reason := a.FallbackFor("n")
	require.Equal(t, "ab", reason)
}

func TestSanitizeReason(t *testing.T) {
	require.Equal(t, "Busy Here", sanitizeReason("Busy Here"))
	require.Equal(t, "ab", sanitizeReason("a\r\nb"))
	require.Equal(t, "ab", sanitizeReason("a\x00\x1f\x7fb"))
}

func TestAdmissionRelease(t *testing.T) {
	ch := make(chan map[string]any, 1)
	var path, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		w.WriteHeader(204)
		ch <- m
	}))
	defer srv.Close()
	newTestAdmission(srv.URL+"/adm").Release("c1", true, 603)
	select {
	case m := <-ch:
		require.Equal(t, "/adm/release", path)
		require.Equal(t, "Bearer k", auth)
		require.Equal(t, "c1", m["call_id"])
		require.Equal(t, true, m["refer_failed"])
		require.EqualValues(t, 603, m["sip_code"])
	case <-time.After(3 * time.Second):
		t.Fatal("no release request")
	}
}

func TestAdmissionDecideRejectsInvalid(t *testing.T) {
	cases := map[string]string{
		"empty":           `{}`,
		"unknown":         `{"action":"nope","fallback":{"timeout_ms":300,"sip_code":480,"reason":"x"}}`,
		"redirect no tgt": `{"action":"redirect","fallback":{"timeout_ms":300,"sip_code":480,"reason":"x"}}`,
		"refer no tgt":    `{"action":"refer","fallback":{"timeout_ms":300,"sip_code":480,"reason":"x"}}`,
		"reject 302":      `{"action":"reject","sip_code":302,"fallback":{"timeout_ms":300,"sip_code":480,"reason":"x"}}`,
		"reject no code":  `{"action":"reject","fallback":{"timeout_ms":300,"sip_code":480,"reason":"x"}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(body))
			}))
			defer srv.Close()
			a := newTestAdmission(srv.URL)
			_, err := a.Decide(context.Background(), AdmissionRequest{To: "n"})
			require.Error(t, err)
			code, _ := a.FallbackFor("n")
			require.Equal(t, 503, code)
		})
	}
}
