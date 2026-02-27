# One-Way Audio on Rapid Consecutive Outbound Calls

## Problem

When two outbound SIP calls are made to the **same number** via the **same trunk** in rapid succession (< 5 seconds apart), the second call may experience **one-way audio**: the remote party hears the agent, but the agent receives zero RTP packets from the remote party.

## Observed Behavior (2026-02-26)

Two outbound calls to `0978963262` via trunk `ST_k9YHY3vKDGk2` to PBX `192.168.150.70:5060`:

| | Call 1 (`room_3d394f1d`) | Call 2 (`room_98aeaf71`) |
|---|---|---|
| Room created | 05:32:27 | 05:32:59 |
| SIP INVITE sent | 05:32:29 | 05:33:01 |
| Call answered (200 OK) | 05:32:37 | 05:33:05 |
| RTP packets received | **1074** | **0** |
| STT audio duration | 2.68s | **0.0s** |
| User hung up | 05:32:58 | 05:33:31 |
| BYE Timer J expired | 05:33:30 | — |

Call 2's INVITE was sent **3 seconds** after Call 1's BYE, while Call 1's BYE server transaction was still alive (SIP Timer J = 32s for UDP).

## Root Cause

The remote PBX (`192.168.150.70`) answered Call 2 at the SIP signaling level (sent 200 OK with SDP on port 10410) but **never started sending RTP** to the SIP service's media port (15297). The PBX's media engine likely had not fully released Call 1's resources.

Evidence from SIP media stats:

```
# Call 1 (working)
port.streams: 1, port.packets: 1074, audio_in_frames: 1074
setting media source: [::ffff:192.168.150.70]:10408   ← RTP received
accepting RTP stream: ssrc 252923897                   ← stream accepted

# Call 2 (broken)
port.streams: 0, port.packets: 0, audio_in_frames: 0
setting media source: invalid AddrPort                 ← only at teardown, never set during call
                                                       ← NO "accepting RTP stream" log
```

## SIP Timer J Context

When the SIP service receives a BYE from the remote PBX:

1. `AcceptBye()` sends 200 OK immediately and marks the call as closed
2. The sipgo library keeps the BYE **server transaction** alive for Timer J (32s, per RFC 3261 Section 17.2.2) to absorb potential retransmissions
3. The BYE server transaction is not explicitly terminated in `AcceptBye()` (`outbound.go:968-973`)

This is standard SIP behavior and does not cause the issue directly. The problem is on the **remote PBX side** — it answered Call 2 but failed to allocate/bind RTP resources.

## ICE Connections

ICE connections between the SIP service and LiveKit server were verified to be consistent across both calls:

- Same IP pattern: LiveKit `10.156.228.40`, SIP publisher `10.156.228.37`, SIP subscriber `100.76.192.1`
- Both connected via UDP with similar connect times (~95ms vs ~102ms)
- WebRTC media path (SIP ↔ LiveKit) was healthy in both calls — the agent's audio reached the phone in Call 2

The break is exclusively on the SIP/RTP path between the SIP service and the remote PBX.

## Mitigation

Since the root cause is on the remote PBX, the fix should be applied at the **application layer** that initiates outbound calls:

- Add a minimum delay (3-5 seconds) between consecutive calls to the same number on the same trunk
- Or implement a per-trunk/per-number cooldown after a call ends

## Optional Cleanup

Consider adding `tx.Terminate()` in `AcceptBye()` to immediately free the BYE server transaction instead of waiting 32s for Timer J:

```go
// outbound.go:968
func (c *sipOutbound) AcceptBye(req *sip.Request, tx sip.ServerTransaction) {
    _ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
    tx.Terminate() // skip Timer J, free transaction immediately
    c.mu.Lock()
    defer c.mu.Unlock()
    c.drop()
}
```

This won't fix the PBX issue but reduces resource usage and avoids overlapping transaction state. The trade-off is that BYE retransmissions won't be handled by the transaction layer (acceptable on low-loss internal networks).
