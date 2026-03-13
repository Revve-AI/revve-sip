# Plan: Implement Cold and Warm Transfer

## Current State

### What exists (Cold/Blind Transfer):
- `TransferSIPParticipant` RPC endpoint accepts `SipCallId`, `TransferTo`, `Headers`, `PlayDialtone`
- `transferCall()` on both `inboundCall` and `outboundCall` sends SIP REFER to remote party
- NOTIFY tracking waits for transfer result (100 Trying → 200 OK or failure)
- Dialtone playback during transfer (mutes room audio, plays ETSI ringing to SIP)
- Analytics: `StartTransfer()` / `EndTransfer()` with status tracking
- Integration tests cover the happy path and failure path
- **This is fully functional cold transfer** — it tells the remote SIP peer "go connect to this other number"

### What's missing (Warm/Attended Transfer):
- No hold mechanism (no re-INVITE with `a=sendonly` or `c=0.0.0.0`)
- No consultation leg (can't dial transfer target to talk before completing)
- No way to bridge original caller with transfer target
- No music-on-hold for the held party
- No RPC to distinguish cold vs warm transfer
- No ability to cancel a warm transfer and return to original call

---

## Architecture

### Cold Transfer (enhance existing)
The current implementation works. Enhancements:
1. Add a `TransferType` field to the RPC request to explicitly mark as cold
2. Add hold-before-transfer option (put caller on hold, then REFER)

### Warm Transfer (new flow)
Warm transfer requires a **multi-step orchestration** at the SIP service level:

```
Step 1: App calls WarmTransfer RPC with (callID, transferTo)
Step 2: SIP service puts original caller on HOLD (re-INVITE with a=sendonly + MOH audio)
Step 3: SIP service opens a NEW outbound SIP call (consultation leg) to transferTo
Step 4: Agent/initiator talks to transfer target via LiveKit room
Step 5a: App calls CompleteTransfer → SIP REFER to bridge caller with target, BYE consultation leg
Step 5b: App calls CancelTransfer → BYE consultation leg, un-HOLD original caller
```

**Key insight**: The warm transfer consultation leg is a second SIP call. The SIP service manages two concurrent calls. The LiveKit room is the bridge — the agent hears both (or one at a time).

---

## Implementation Steps

### Step 1: Add SIP Hold/Unhold (re-INVITE)

**Files**: `pkg/sip/inbound.go`, `pkg/sip/outbound.go`, `pkg/sip/media_port.go`, `pkg/sip/protocol.go`

1. Add `sendHoldReInvite()` method to `sipInbound` and `sipOutbound`:
   - Build re-INVITE with modified SDP: set `a=sendonly` direction attribute
   - Send re-INVITE, wait for 200 OK, send ACK
   - Mute outbound audio on `MediaPort` (already have `DisableOut()`)

2. Add `sendUnholdReInvite()` method:
   - Build re-INVITE with `a=sendrecv` to restore bidirectional audio
   - Send re-INVITE, wait for 200 OK, send ACK
   - Re-enable outbound audio via `EnableOut()`

3. Add `holdCall()` / `unholdCall()` on `inboundCall` and `outboundCall`:
   - Track hold state with `atomic.Bool`
   - On hold: `SwapOutput(nil)` to stop room→SIP audio, play MOH via `media.GetAudioWriter()`
   - On unhold: `SwapOutput(media.GetAudioWriter())` to restore room→SIP audio

4. Add MOH audio:
   - Add a simple MOH tone generator in `pkg/sip/moh.go` (sine wave loop or use existing `tones` package)
   - Or load a short audio file from `res/` directory

### Step 2: New RPC Endpoints

**Files**: `pkg/sip/service.go`, `pkg/service/service.go`, `pkg/service/psrpc.go`

Since the RPC types come from `livekit/protocol` (external dependency), we'll extend behavior using the **existing `TransferSIPParticipant` RPC** with new header conventions, or add new service-internal methods:

1. **Option A (Preferred — no proto changes)**: Use `Headers` map in existing `InternalTransferSIPParticipantRequest`:
   - `X-Transfer-Type: warm` → initiates warm transfer (hold + consult leg)
   - `X-Transfer-Type: cold` → existing behavior (default)
   - After warm transfer is initiated, the consultation call gets its own `SipCallId`
   - Complete/cancel via a second `TransferSIPParticipant` call with:
     - `X-Transfer-Action: complete` on the original call
     - `X-Transfer-Action: cancel` on the original call

2. **Option B (Clean but requires proto)**: Add new RPC methods:
   - `HoldSIPParticipant(callID)` → put on hold
   - `UnholdSIPParticipant(callID)` → resume
   - `WarmTransferSIPParticipant(callID, transferTo)` → hold + dial consult
   - `CompleteWarmTransfer(callID)` → REFER + BYE consult
   - `CancelWarmTransfer(callID)` → BYE consult + unhold

**Recommendation**: Start with **Option A** (header-based) since it requires no proto changes and can ship faster. Migrate to Option B later if needed.

### Step 3: Warm Transfer State Machine

**Files**: `pkg/sip/warm_transfer.go` (new), `pkg/sip/service.go`

1. Create `WarmTransfer` struct to track:
   ```go
   type WarmTransfer struct {
       mu              sync.Mutex
       originalCallID  LocalTag        // the held call
       consultCallID   LocalTag        // the outbound consultation leg
       transferTo      string          // destination
       state           WarmTransferState // initiated, consulting, completing, cancelled
       consultCall     *outboundCall   // reference to consultation call
       mohCancel       context.CancelFunc
   }
   ```

2. States: `WarmTransferInitiated` → `WarmTransferConsulting` → `WarmTransferCompleting` / `WarmTransferCancelled`

3. Add `activeWarmTransfers map[LocalTag]*WarmTransfer` to `Service`

### Step 4: Warm Transfer Flow Implementation

**Files**: `pkg/sip/service.go`, `pkg/sip/inbound.go`, `pkg/sip/outbound.go`

#### Initiate (hold + dial consultation):
1. Find original call (inbound or outbound)
2. Put original call on hold: `holdCall()` → re-INVITE sendonly + start MOH
3. Create consultation outbound call to `transferTo`:
   - Join same LiveKit room (agent can now talk to transfer target)
   - Or join a temporary room for private consultation
4. Store `WarmTransfer` state
5. Return consultation call's `SipCallId` to app

#### Complete (bridge + disconnect):
1. Find `WarmTransfer` by original call ID
2. Send SIP REFER on original call to `transferTo` (same as cold transfer)
3. Wait for REFER NOTIFY success
4. BYE the consultation leg
5. Clean up state

#### Cancel (hangup consult + resume original):
1. Find `WarmTransfer` by original call ID
2. BYE the consultation leg
3. Unhold original call: `unholdCall()` → re-INVITE sendrecv + stop MOH
4. Restore room audio: `SwapOutput(media.GetAudioWriter())`
5. Clean up state

### Step 5: Analytics and State Tracking

**Files**: `pkg/sip/analytics.go`

1. Add warm transfer states to `CallState`:
   - `CallHold` state (already defined in proto but unused)
   - Transfer type tracking (cold vs warm)
2. Report hold/unhold events via `UpdateSIPCallState`
3. Track consultation leg as linked call

### Step 6: Tests

**Files**: `pkg/sip/service_test.go`, `test/integration/sip_test.go`

1. Unit tests:
   - Hold/unhold re-INVITE generation and SDP manipulation
   - Warm transfer state machine transitions
   - Cancel warm transfer restores original call

2. Integration tests:
   - Cold transfer (existing, verify still works)
   - Warm transfer full flow: hold → consult → complete
   - Warm transfer cancel: hold → consult → cancel → resume
   - Warm transfer with consultation failure (target doesn't answer)

---

## File Change Summary

| File | Changes |
|---|---|
| `pkg/sip/protocol.go` | Add `NewReInviteRequest()` for hold/unhold SDP |
| `pkg/sip/media_port.go` | Add hold state tracking, MOH writer |
| `pkg/sip/moh.go` (new) | Music-on-hold tone generator |
| `pkg/sip/warm_transfer.go` (new) | WarmTransfer state machine |
| `pkg/sip/inbound.go` | Add `holdCall()`, `unholdCall()`, modify `transferCall()` |
| `pkg/sip/outbound.go` | Add `holdCall()`, `unholdCall()`, modify `transferCall()` |
| `pkg/sip/service.go` | Route warm transfer RPCs, manage `activeWarmTransfers` |
| `pkg/sip/analytics.go` | Track hold state and transfer type |
| `pkg/sip/service_test.go` | Unit tests for warm transfer |
| `test/integration/sip_test.go` | Integration tests for both transfer types |

---

## Risks and Mitigations

1. **re-INVITE compatibility**: Some SIP providers don't handle re-INVITE well → implement fallback where hold is done at media level only (mute RTP, play MOH) without re-INVITE
2. **Proto dependency**: `InternalTransferSIPParticipantRequest` is in `livekit/protocol` → use header-based approach (Option A) to avoid proto changes
3. **Race conditions**: Two concurrent transfers on same call → mutex protection in WarmTransfer, reject second transfer if one is active
4. **Consultation leg room placement**: Agent needs to hear transfer target → use same LiveKit room, or a side room with agent bridged to both
