package sip

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/media-sdk/sdp"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/sipgo"
	"github.com/livekit/sipgo/sip"

	"github.com/livekit/sip/pkg/config"
)

type fakeAdmission struct {
	mu       sync.Mutex
	decide   func(w http.ResponseWriter, r *http.Request)
	releases []admissionReleaseRequest
	srv      *httptest.Server
}

func newFakeAdmission(t *testing.T, decide func(w http.ResponseWriter, r *http.Request)) *fakeAdmission {
	f := &fakeAdmission{decide: decide}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/release" {
			var req admissionReleaseRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.mu.Lock()
			f.releases = append(f.releases, req)
			f.mu.Unlock()
			return
		}
		f.decide(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAdmission) releaseCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.releases)
}

func writeDecision(w http.ResponseWriter, dec map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(dec)
}

func admissionHandler() *TestHandler {
	return &TestHandler{
		GetAuthCredentialsFunc: func(ctx context.Context, call *rpc.SIPCall) (AuthInfo, error) {
			return AuthInfo{Result: AuthAccept}, nil
		},
		DispatchCallFunc: func(ctx context.Context, info *CallInfo) CallDispatch {
			return CallDispatch{
				Result:         DispatchAccept,
				Room:           RoomConfig{RoomName: "r", Participant: ParticipantConfig{Identity: "p"}},
				RingingTimeout: 30 * time.Second,
			}
		},
	}
}

func withAdmission(url string) func(*config.Config) {
	return func(c *config.Config) {
		c.SIPRingingInterval = time.Second
		c.Admission = &config.AdmissionConfig{URL: url, APIKey: "k", DefaultTimeout: 2 * time.Second, DefaultFallbackCode: 503}
	}
}

func expectFinal(t *testing.T, tx sip.ClientTransaction) *sip.Response {
	res := getResponseOrFail(t, tx)
	require.Equal(t, sip.StatusCode(100), res.StatusCode)
	return getResponseOrFail(t, tx)
}

func requireReleases(t *testing.T, f *fakeAdmission, n int) {
	require.Eventually(t, func() bool { return f.releaseCount() == n }, 2*time.Second, 20*time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	require.Equal(t, n, f.releaseCount())
}

func TestAdmission_Off(t *testing.T) {
	h := &TestHandler{GetAuthCredentialsFunc: func(ctx context.Context, call *rpc.SIPCall) (AuthInfo, error) {
		return AuthInfo{Result: AuthNotFound}, nil
	}}
	testInviteWithConf(t, h, false, "a", "b", nil, func(tx sip.ClientTransaction) {
		require.Equal(t, sip.StatusCode(404), expectFinal(t, tx).StatusCode)
	})
}

func TestAdmission_Reject(t *testing.T) {
	f := newFakeAdmission(t, func(w http.ResponseWriter, r *http.Request) {
		writeDecision(w, map[string]any{"decision_id": "d1", "action": "reject", "sip_code": 486, "reason": "Busy Here"})
	})
	testInviteWithConf(t, admissionHandler(), false, "a", "b", withAdmission(f.srv.URL), func(tx sip.ClientTransaction) {
		res := expectFinal(t, tx)
		require.Equal(t, sip.StatusCode(486), res.StatusCode)
		require.Equal(t, "Busy Here", res.Reason)
	})
	requireReleases(t, f, 1)
}

func TestAdmission_RejectCustomCode(t *testing.T) {
	f := newFakeAdmission(t, func(w http.ResponseWriter, r *http.Request) {
		writeDecision(w, map[string]any{"decision_id": "d1", "action": "reject", "sip_code": 499, "reason": "Custom Why"})
	})
	testInviteWithConf(t, admissionHandler(), false, "a", "b", withAdmission(f.srv.URL), func(tx sip.ClientTransaction) {
		res := expectFinal(t, tx)
		require.Equal(t, sip.StatusCode(499), res.StatusCode)
		require.Equal(t, "Custom Why", res.Reason)
	})
}

func TestAdmission_Redirect(t *testing.T) {
	f := newFakeAdmission(t, func(w http.ResponseWriter, r *http.Request) {
		writeDecision(w, map[string]any{"decision_id": "d1", "action": "redirect", "target": "sip:+84901234567@carrier.example.com"})
	})
	testInviteWithConf(t, admissionHandler(), false, "a", "b", withAdmission(f.srv.URL), func(tx sip.ClientTransaction) {
		res := expectFinal(t, tx)
		require.Equal(t, sip.StatusCode(302), res.StatusCode)
		require.Len(t, res.GetHeaders("Contact"), 1)
		c := res.Contact()
		require.Equal(t, "+84901234567", c.Address.User)
		require.Equal(t, "carrier.example.com", c.Address.Host)
	})
	requireReleases(t, f, 1)
}

func TestAdmission_Down(t *testing.T) {
	f := newFakeAdmission(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	testInviteWithConf(t, admissionHandler(), false, "a", "b", withAdmission(f.srv.URL), func(tx sip.ClientTransaction) {
		require.Equal(t, sip.StatusCode(503), expectFinal(t, tx).StatusCode)
	})
}

func TestAdmission_ReferIsFallbackForNow(t *testing.T) {
	f := newFakeAdmission(t, func(w http.ResponseWriter, r *http.Request) {
		writeDecision(w, map[string]any{"decision_id": "d1", "action": "refer", "target": "sip:x@y.example.com"})
	})
	testInviteWithConf(t, admissionHandler(), false, "a", "b", withAdmission(f.srv.URL), func(tx sip.ClientTransaction) {
		require.Equal(t, sip.StatusCode(503), expectFinal(t, tx).StatusCode)
	})
}

func TestAdmission_CachedFallbackThenDown(t *testing.T) {
	var n atomic.Int32
	f := newFakeAdmission(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			writeDecision(w, map[string]any{
				"decision_id": nil, "action": "reject", "sip_code": 486,
				"fallback": map[string]any{"timeout_ms": 1000, "sip_code": 480, "reason": "Temporarily Unavailable"},
			})
			return
		}
		w.WriteHeader(500)
	})
	// The fallback cache lives in the server, so the second INVITE goes to the same service.
	var sipPort int
	mutate := func(c *config.Config) {
		withAdmission(f.srv.URL)(c)
		sipPort = c.SIPPort
	}
	testInviteWithConf(t, admissionHandler(), false, "a", "b", mutate, func(tx sip.ClientTransaction) {
		require.Equal(t, sip.StatusCode(486), expectFinal(t, tx).StatusCode)

		ua, err := sipgo.NewUA(sipgo.WithUserAgent("a"))
		require.NoError(t, err)
		client, err := sipgo.NewClient(ua)
		require.NoError(t, err)
		localIP, err := config.GetLocalIP()
		require.NoError(t, err)
		addr := fmt.Sprintf("%s:%d", localIP, sipPort)
		offer, err := sdp.NewOffer(localIP, 0xB0B, sdp.EncryptionNone)
		require.NoError(t, err)
		offerData, err := offer.SDP.Marshal()
		require.NoError(t, err)
		req := sip.NewRequest(sip.INVITE, sip.Uri{User: "b", Host: addr})
		req.SetDestination(addr)
		req.SetBody(offerData)
		req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		tx2, err := client.TransactionRequest(req)
		require.NoError(t, err)
		t.Cleanup(tx2.Terminate)
		require.Equal(t, sip.StatusCode(480), expectFinal(t, tx2).StatusCode)
	})
}

func TestAdmission_Accept(t *testing.T) {
	var mu sync.Mutex
	var gotReq AdmissionRequest
	f := newFakeAdmission(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		mu.Unlock()
		writeDecision(w, map[string]any{"decision_id": "d1", "action": "accept", "call_bot_id": "bot1"})
	})
	testInviteWithConf(t, admissionHandler(), false, "alice", "bob", withAdmission(f.srv.URL), func(tx sip.ClientTransaction) {
		require.Equal(t, sip.StatusCode(100), getResponseOrFail(t, tx).StatusCode)
		require.Equal(t, sip.StatusCode(180), getResponseOrFail(t, tx).StatusCode)
	})
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, "bob", gotReq.To)
	require.NotEmpty(t, gotReq.CallID)
	require.NotEmpty(t, gotReq.SIPCallID)
	require.NotEmpty(t, gotReq.Headers["call-id"])
}

func TestAdmission_CancelDuringAdmission(t *testing.T) {
	f := newFakeAdmission(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(1500 * time.Millisecond):
			writeDecision(w, map[string]any{"decision_id": "d1", "action": "accept"})
		case <-r.Context().Done():
		}
	})
	testInviteWithConf(t, admissionHandler(), false, "a", "b", withAdmission(f.srv.URL), func(tx sip.ClientTransaction) {
		require.Equal(t, sip.StatusCode(100), getResponseOrFail(t, tx).StatusCode)
		require.NoError(t, tx.Cancel())
		res := getResponseOrFail(t, tx)
		require.Equal(t, sip.StatusCode(487), res.StatusCode)
		require.Equal(t, sip.INVITE, res.CSeq().MethodName)
		expectNoResponse(t, tx)
	})
}

func TestContactForTarget(t *testing.T) {
	c, err := contactForTarget("sip:queue@pbx.example.com", "host.example.com")
	require.NoError(t, err)
	require.Equal(t, "queue", c.Address.User)
	require.Equal(t, "pbx.example.com", c.Address.Host)

	for _, target := range []string{"+842873001234", "tel:+842873001234"} {
		c, err = contactForTarget(target, "host.example.com")
		require.NoError(t, err, target)
		require.Equal(t, "+842873001234", c.Address.User, target)
		require.Equal(t, "host.example.com", c.Address.Host, target)
	}

	for _, target := range []string{"http://x", "queue", ""} {
		_, err = contactForTarget(target, "host.example.com")
		require.Error(t, err, target)
	}
}
