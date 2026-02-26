# Media Quality & Latency Logs

This document describes the logs added for monitoring SIP provider quality, call latency, and media errors during active calls.

## Call Latency Logs

These logs measure how long key SIP signaling steps take. They are equivalent to HTTP request latency but for SIP calls.

### `SIP check duration`

Measures the time from sending/receiving INVITE to getting the first response from the provider.

**When**: Logged once per call, after the initial signaling exchange completes.

| Direction | What it measures |
|-----------|------------------|
| Inbound | INVITE received to dispatch response (trunk match + auth) |
| Outbound | INVITE sent to provider 200 OK response |

**Log fields**:

| Field | Type | Description |
|-------|------|-------------|
| `dur_check` | `time.Duration` | Signaling round-trip time |

**Example**:
```
INFO  SIP check duration (INVITE to provider response)  dur_check=245ms
```

**Thresholds**:

| Value | Status |
|-------|--------|
| < 500ms | Healthy |
| 500ms - 2s | Degraded - provider is slow |
| > 2s | Bad - provider may be overloaded or unreachable |

---

### `SIP join duration`

Measures the time from INVITE to the full media path being ready.

**When**: Logged once per call, when media is fully established.

| Direction | What it measures |
|-----------|------------------|
| Inbound | INVITE received to room audio mixed and flowing |
| Outbound | INVITE sent to ACK sent (full media path ready) |

**Log fields**:

| Field | Type | Description |
|-------|------|-------------|
| `dur_join` | `time.Duration` | Total setup time until audio flows |

**Example**:
```
INFO  SIP join duration (INVITE to ACK)  dur_join=1.2s
```

**Thresholds**:

| Value | Status |
|-------|--------|
| < 2s | Healthy |
| 2s - 5s | Degraded |
| > 5s | Bad - users will notice delay before hearing audio |

---

## Periodic Media Quality Log

Logs media quality metrics every **30 seconds** during an active call. Tracks the delta (change) since the last report, not cumulative totals.

**Log level**: `INFO` when healthy, auto-escalates to `WARN` when degraded (packet loss > 5%, RTP resets, or write errors).

**Example (healthy)**:
```
INFO  media quality  interval=30s  rxPackets=1500  packetLoss=0  packetLossRate=0.0%
  latePackets=0  delayedPackets=2  avgDelayMs=25.0  rapidPackets=0
  rtpResets=0  writeErrors=0  audioRxHz=48000  audioTxHz=48000  gaps=0
```

**Example (degraded)**:
```
WARN  media quality degraded  interval=30s  rxPackets=1200  packetLoss=45  packetLossRate=3.6%
  latePackets=8  delayedPackets=30  avgDelayMs=85.3  rapidPackets=5
  rtpResets=1  writeErrors=3  audioRxHz=47200  audioTxHz=48000  gaps=12
```

### Fields

| Field | Type | Description |
|-------|------|-------------|
| `interval` | `duration` | Reporting interval (30s) |
| `rxPackets` | `uint64` | RTP packets received in this interval |
| `packetLoss` | `uint64` | Number of lost packets (detected via RTP sequence gaps) |
| `packetLossRate` | `string` | Packet loss as percentage of total expected packets |
| `latePackets` | `uint64` | Packets that arrived out of order |
| `delayedPackets` | `uint64` | Packets that arrived later than 1.5x the expected 20ms interval (> 30ms) |
| `avgDelayMs` | `string` | Average inter-packet delay for delayed packets (ms) |
| `rapidPackets` | `uint64` | Packets that arrived faster than 0.5x the expected interval (< 10ms), indicates burst delivery |
| `rtpResets` | `uint64` | RTP stream resets (large sequence number jumps), indicates stream interruption |
| `writeErrors` | `uint64` | Failed RTP packet sends to provider |
| `audioRxHz` | `string` | Actual incoming audio sample rate (expected: 48000) |
| `audioTxHz` | `string` | Actual outgoing audio sample rate (expected: 48000) |
| `gaps` | `uint64` | Number of gap events (each gap may contain multiple lost packets) |

### Warn escalation triggers

The log level changes from `INFO` to `WARN` when any of these conditions are true:

- `packetLossRate` > 5%
- `rtpResets` > 0
- `writeErrors` > 0

### Interpreting results for provider quality

| Symptom | Log indicators | Likely cause |
|---------|---------------|--------------|
| User hears audio late | High `delayedPackets`, high `avgDelayMs` | Provider network jitter / high latency |
| User hears choppy audio | High `packetLoss`, high `gaps` | Provider dropping packets |
| Audio cuts out briefly | `rtpResets` > 0 | Provider stream interrupted |
| User hears nothing | `rxPackets` = 0, or `writeErrors` high | Provider down or network routing issue |
| Audio sounds garbled | High `latePackets`, high `rapidPackets` | Severe jitter, packets reordered |
| Agent talks but user gets it late | High `writeErrors`, `audioTxHz` far from 48000 | Cannot send RTP to provider / encoding backpressure |

---

## RTP Write Error Log

Logged when sending an RTP packet to the SIP provider fails. Rate-limited to **1 log per second** to prevent flooding.

**Log level**: `WARN`

**Example**:
```
WARN  RTP write error  error="write udp: connection refused"  writeErrors=3  payloadType=111  seq=1234
```

### Fields

| Field | Type | Description |
|-------|------|-------------|
| `error` | `error` | The underlying network error |
| `writeErrors` | `uint64` | Cumulative write error count for this call |
| `payloadType` | `uint8` | RTP payload type (e.g. 111 for Opus) |
| `seq` | `uint16` | RTP sequence number of the failed packet |

---

## UDP Media Write Error Log

Logged when the low-level UDP socket write fails. Rate-limited to **1 log per second**.

**Log level**: `WARN`

**Example**:
```
WARN  UDP media write error  error="write: no route to host"  dst=10.0.0.5:20000  writeErrors=5
```

### Fields

| Field | Type | Description |
|-------|------|-------------|
| `error` | `error` | The OS-level socket error |
| `dst` | `string` | Destination IP:port of the SIP provider |
| `writeErrors` | `uint64` | Cumulative UDP write error count |

---

## End-of-Call Statistics

The existing `call statistics` log at call end now includes `write_errors` in the `stats.port` JSON object.

**Example**:
```
INFO  call statistics  durMin=5  sip_rx_ppm=+120  sip_tx_ppm=-50  stats={"port":{"write_errors":0,...},...}
```

---

## Files Changed

| File | Change |
|------|--------|
| `pkg/sip/inbound.go` | Added `dur_check` and `dur_join` logging for inbound calls |
| `pkg/sip/outbound.go` | Added `dur_check` and `dur_join` logging for outbound calls |
| `pkg/sip/media_port.go` | Added `mediaQualityLoop()` (30s periodic quality log), UDP write error logging, `WriteErrors` in stats |
| `pkg/sip/media.go` | Added RTP write error logging in `rtpStatsWriter` |
