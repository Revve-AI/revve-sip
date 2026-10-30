package sip

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"

	"github.com/livekit/protocol/logger"
	"github.com/livekit/sip/pkg/config"
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

func (a *admissionClient) setFallback(to string, timeoutMs, code int, reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
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
