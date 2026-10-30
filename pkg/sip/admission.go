package sip

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"

	// livekit/sipgo's sip.Uri is an alias of emiago's Uri and has no parser of its own.
	// A livekit/sipgo bump that drops the alias would break contactForTarget.
	emiagosip "github.com/emiago/sipgo/sip"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/psrpc"
	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/stats"
	"github.com/livekit/sipgo/sip"
)

const (
	admissionMinTimeout     = 200 * time.Millisecond
	admissionMaxTimeout     = 5 * time.Second
	admissionReleaseTimeout = 2 * time.Second
)

type AdmissionRequest struct {
	CallID    string            `json:"call_id"`
	SIPCallID string            `json:"sip_call_id"`
	From      string            `json:"from"`
	To        string            `json:"to"`
	Headers   map[string]string `json:"headers"`
	SourceIP  string            `json:"source_ip"`
	TrunkID   string            `json:"trunk_id"`
}

type AdmissionFallback struct {
	TimeoutMs int    `json:"timeout_ms"`
	SIPCode   int    `json:"sip_code"`
	Reason    string `json:"reason"`
}

type AdmissionDecision struct {
	DecisionID *string           `json:"decision_id"`
	Action     string            `json:"action"` // accept, reject, redirect, refer
	SIPCode    int               `json:"sip_code,omitempty"`
	Reason     string            `json:"reason,omitempty"`
	Target     string            `json:"target,omitempty"`
	CallBotID  string            `json:"call_bot_id,omitempty"`
	Fallback   AdmissionFallback `json:"fallback"`
}

type admissionReleaseRequest struct {
	CallID      string `json:"call_id"`
	ReferFailed bool   `json:"refer_failed,omitempty"`
	SIPCode     int    `json:"sip_code,omitempty"`
}

type admissionFallback struct {
	timeout time.Duration
	code    int
	reason  string
}

type admissionClient struct {
	http *http.Client
	conf *config.AdmissionConfig
	log  logger.Logger

	mu       sync.Mutex
	fallback map[string]admissionFallback
}

func newAdmissionClient(conf *config.AdmissionConfig) *admissionClient {
	if conf == nil || conf.URL == "" {
		return nil
	}
	return &admissionClient{
		http:     &http.Client{},
		conf:     conf,
		log:      logger.GetLogger(),
		fallback: make(map[string]admissionFallback),
	}
}

// sanitizeReason strips CR, LF and other control characters from a SIP reason phrase.
func sanitizeReason(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

func clampAdmissionTimeout(d time.Duration) time.Duration {
	if d < admissionMinTimeout {
		return admissionMinTimeout
	}
	if d > admissionMaxTimeout {
		return admissionMaxTimeout
	}
	return d
}

// setFallback caches the rule fallback for the dialed number. Zero values mean the number
// has no rule, so the entry is dropped and the config defaults apply.
func (a *admissionClient) setFallback(to string, timeoutMs, code int, reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if timeoutMs <= 0 && code == 0 {
		delete(a.fallback, to)
		return
	}
	a.fallback[to] = admissionFallback{
		timeout: time.Duration(timeoutMs) * time.Millisecond,
		code:    code,
		reason:  sanitizeReason(reason),
	}
}

func (a *admissionClient) timeoutFor(to string) time.Duration {
	a.mu.Lock()
	fb, ok := a.fallback[to]
	a.mu.Unlock()
	if ok && fb.timeout > 0 {
		return clampAdmissionTimeout(fb.timeout)
	}
	return clampAdmissionTimeout(a.conf.DefaultTimeout)
}

// FallbackFor returns the SIP code and reason to use when admission fails for the dialed number.
func (a *admissionClient) FallbackFor(to string) (int, string) {
	a.mu.Lock()
	fb, ok := a.fallback[to]
	a.mu.Unlock()
	if ok && fb.code != 0 {
		reason := fb.reason
		if reason == "" {
			reason = defaultFallbackReason(fb.code)
		}
		return fb.code, reason
	}
	code := a.conf.DefaultFallbackCode
	if code == 0 {
		code = 503
	}
	return code, defaultFallbackReason(code)
}

func defaultFallbackReason(code int) string {
	if code == 503 {
		return "Service Unavailable"
	}
	return sipStatus(sip.StatusCode(code))
}

func (a *admissionClient) post(ctx context.Context, url string, body any) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.conf.APIKey)
	return a.http.Do(req)
}

// Decide asks revve-web whether to answer the call.
func (a *admissionClient) Decide(ctx context.Context, req AdmissionRequest) (AdmissionDecision, error) {
	var dec AdmissionDecision
	ctx, cancel := context.WithTimeout(ctx, a.timeoutFor(req.To))
	defer cancel()
	resp, err := a.post(ctx, a.conf.URL, req)
	if err != nil {
		return dec, errors.Wrap(err, "admission request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return dec, fmt.Errorf("admission returned status %d", resp.StatusCode)
	}
	if err = json.NewDecoder(resp.Body).Decode(&dec); err != nil {
		return AdmissionDecision{}, errors.Wrap(err, "admission response decode failed")
	}
	if err = validateAdmissionDecision(dec); err != nil {
		return AdmissionDecision{}, err
	}
	dec.Reason = sanitizeReason(dec.Reason)
	dec.Fallback.Reason = sanitizeReason(dec.Fallback.Reason)
	a.setFallback(req.To, dec.Fallback.TimeoutMs, dec.Fallback.SIPCode, dec.Fallback.Reason)
	return dec, nil
}

// Release tells revve-web the call ended or the transfer failed. It never blocks the caller.
func (a *admissionClient) Release(callID string, referFailed bool, sipCode int) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), admissionReleaseTimeout)
		defer cancel()
		resp, err := a.post(ctx, strings.TrimRight(a.conf.URL, "/")+"/release",
			admissionReleaseRequest{CallID: callID, ReferFailed: referFailed, SIPCode: sipCode})
		if err == nil {
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			if resp.StatusCode < 200 || resp.StatusCode > 299 {
				err = fmt.Errorf("status %d", resp.StatusCode)
			}
		}
		if err != nil {
			a.log.Warnw("admission release failed", err, "callID", callID)
		}
	}()
}

func validateAdmissionDecision(dec AdmissionDecision) error {
	switch dec.Action {
	case "accept":
	case "reject":
		if dec.SIPCode < 400 || dec.SIPCode > 699 {
			return fmt.Errorf("admission reject has invalid sip_code %d", dec.SIPCode)
		}
	case "redirect", "refer":
		if dec.Target == "" {
			return fmt.Errorf("admission %s has no target", dec.Action)
		}
	default:
		return fmt.Errorf("admission returned unknown action %q", dec.Action)
	}
	return nil
}

func admissionHeaders(hs Headers) map[string]string {
	out := make(map[string]string, len(hs))
	for _, h := range hs {
		name := strings.ToLower(h.Name())
		if _, ok := out[name]; !ok {
			out[name] = h.Value()
		}
	}
	return out
}

// contactForTarget builds the Contact for a 302 from a redirect target.
func contactForTarget(target, toHost string) (*sip.ContactHeader, error) {
	target = strings.TrimSpace(target)
	if strings.HasPrefix(target, "tel:") {
		target = strings.TrimPrefix(target, "tel:")
	}
	if strings.HasPrefix(target, "+") {
		target = "sip:" + target + "@" + toHost
	}
	var uri sip.Uri
	if err := emiagosip.ParseUri(target, &uri); err != nil {
		return nil, err
	}
	if uri.Scheme != "sip" && uri.Scheme != "sips" {
		return nil, fmt.Errorf("unsupported redirect target %q", target)
	}
	return &sip.ContactHeader{Address: uri}, nil
}

// admitInvite asks revve-web whether to answer the call. ok=false means the INVITE was
// already answered (or dropped) and the caller must return err.
func (s *Server) admitInvite(
	ctx context.Context, log logger.Logger, cmon *stats.CallMonitor, cc *sipInbound, tx sip.ServerTransaction,
	callID string, from, to sip.Uri, src netip.AddrPort, trunkID string, rheaders Headers,
) (extra map[string]string, decisionID string, referTarget string, ok bool, err error) {
	a := s.admission
	type result struct {
		dec AdmissionDecision
		err error
	}
	dctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch := make(chan result, 1)
	go func() {
		dec, err := a.Decide(dctx, AdmissionRequest{
			CallID:    callID,
			SIPCallID: cc.SIPCallID(),
			From:      from.User,
			To:        to.User,
			Headers:   admissionHeaders(rheaders),
			SourceIP:  src.Addr().String(),
			TrunkID:   trunkID,
		})
		ch <- result{dec, err}
	}()

	var res result
	select {
	case res = <-ch:
	case creq := <-tx.Cancels():
		cancel()
		close(cc.cancelled)
		_ = tx.Respond(sip.NewResponseFromRequest(creq, sip.StatusOK, "OK", nil))
		cc.RespondAndDrop(sip.StatusRequestTerminated, "Request Terminated")
		cmon.InviteErrorShort("admission-cancelled")
		log.Infow("Caller cancelled while waiting for admission")
		// revve-web may still finish the decision and take slots. Release is idempotent and,
		// before a decision exists, makes revve-web refuse the late admission.
		a.Release(callID, false, 0)
		return nil, "", "", false, psrpc.NewErrorf(psrpc.Canceled, "call cancelled during admission")
	}

	fallback := func(why string, cause error) (map[string]string, string, string, bool, error) {
		code, reason := a.FallbackFor(to.User)
		cmon.InviteErrorShort("admission-error")
		log.Warnw("Rejecting inbound, admission failed", cause, "reason", why, "sipCode", code)
		cc.RespondAndDrop(sip.StatusCode(code), sanitizeReason(reason))
		return nil, "", "", false, psrpc.NewErrorf(psrpc.Unavailable, "admission failed: %s", why)
	}
	if res.err != nil {
		// A timed-out request can still complete in revve-web and hold slots.
		a.Release(callID, false, 0)
		return fallback("error", res.err)
	}
	dec := res.dec
	id := ""
	if dec.DecisionID != nil {
		id = *dec.DecisionID
	}
	release := func(code int) {
		if id != "" {
			a.Release(callID, false, code)
		}
	}
	switch dec.Action {
	case "accept":
		extra = map[string]string{}
		if id != "" {
			extra["revve.routing.decision_id"] = id
		}
		if dec.CallBotID != "" {
			extra["revve.routing.call_bot_id"] = dec.CallBotID
		}
		if len(extra) == 0 {
			extra = nil
		}
		return extra, id, "", true, nil
	case "reject":
		reason := dec.Reason
		if reason == "" {
			reason = sipStatus(sip.StatusCode(dec.SIPCode))
		}
		cmon.InviteErrorShort("admission-reject")
		log.Infow("Rejecting inbound by admission", "sipCode", dec.SIPCode)
		cc.RespondAndDrop(sip.StatusCode(dec.SIPCode), sanitizeReason(reason))
		release(dec.SIPCode)
		return nil, "", "", false, psrpc.NewErrorf(psrpc.PermissionDenied, "call rejected by admission")
	case "redirect":
		contact, cerr := contactForTarget(dec.Target, to.Host)
		if cerr != nil {
			release(0)
			return fallback("bad redirect target", cerr)
		}
		cmon.InviteErrorShort("admission-redirect")
		log.Infow("Redirecting inbound by admission", "target", dec.Target)
		cc.RespondAndDropWithContact(sip.StatusMovedTemporarily, "Moved Temporarily", contact)
		release(int(sip.StatusMovedTemporarily))
		return nil, "", "", false, psrpc.NewErrorf(psrpc.Canceled, "call redirected by admission")
	case "refer":
		contact, cerr := contactForTarget(dec.Target, to.Host)
		if cerr != nil {
			release(0)
			return fallback("bad refer target", cerr)
		}
		return nil, id, contact.Address.String(), true, nil
	default:
		release(0)
		return fallback("unsupported action "+dec.Action, nil)
	}
}
