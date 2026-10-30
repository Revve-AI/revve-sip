package sip

import (
	"context"
	"errors"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils/traceid"
	"github.com/livekit/sipgo/sip"
)

// referNotifyTimeout bounds the wait for the carrier to accept and complete the REFER.
var referNotifyTimeout = 30 * time.Second

// handleReferWithoutRoom answers the call, sends REFER to the carrier and hangs up.
// No LiveKit room is joined, so the caller audio is never consumed.
func (c *inboundCall) handleReferWithoutRoom(ctx context.Context, tid traceid.ID, req *sip.Request, referTo string) error {
	c.mon.InviteAccept()
	c.mon.CallStart()
	defer c.mon.CallEnd()
	defer c.close(ctx, true, callDropped, "other")

	if h := req.CallID(); h != nil {
		c.call.SipCallId = h.Value()
	}
	c.cc.StartRinging()

	ans, err := c.runMediaConn(tid, req.Body(), livekit.SIPMediaEncryption_SIP_MEDIA_ENCRYPT_DISABLE, c.s.conf, nil, nil)
	if err != nil {
		c.log().Errorw("Cannot prepare media for refer", err)
		c.referFailed.Store(true)
		c.referCode.Store(int32(sip.StatusInternalServerError))
		c.cc.RespondAndDrop(sip.StatusInternalServerError, "Internal Server Error")
		return err
	}
	if err = c.cc.Accept(ctx, ans, nil); err != nil {
		c.log().Errorw("Cannot accept call for refer", err)
		// A caller CANCEL or hangup is a normal end, not a failed transfer.
		c.referFailed.Store(c.ctx.Err() == nil)
		return err
	}
	c.media.EnableTimeout(true)

	tctx, cancel := context.WithTimeout(ctx, referNotifyTimeout)
	defer cancel()
	if err = c.cc.TransferCall(tctx, "<"+referTo+">", nil); err != nil {
		code := 0
		var st *livekit.SIPStatus
		if errors.As(err, &st) {
			code = int(st.Code)
		}
		c.log().Infow("refer without room failed", "error", err, "referTo", referTo, "sipCode", code)
		if c.ctx.Err() == nil {
			c.referFailed.Store(true)
			c.referCode.Store(int32(code))
		}
		c.closeWithHangup(ctx)
		return err
	}
	c.log().Infow("call transferred without room", "referTo", referTo)

	// Give the carrier time to hang up first; hang up ourselves after referByeTimeout.
	select {
	case <-time.After(referByeTimeout):
	case <-c.ctx.Done():
	}
	c.closeWithHangup(ctx)
	return nil
}
