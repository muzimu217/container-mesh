# p2p2 KPI log

Measurements on macOS arm64 (Apple M4), Go 1.27.2, UDP loopback, `MTU` fixed at
1200 bytes. Every number is `best of N` across fresh connections, because a
loopback benchmark on a machine that is also compiling has a long tail; the tail
is reported separately where it matters.

The workload is the one the peer layer actually produces: one 512 KiB message,
which is 464 fragments at the 1131-byte payload (478 at the 1099-byte payload
used before round 10).

## Round 0 — baseline of the inherited work-in-progress

The package already existed in the tree when this work started. It built and its
deterministic reassembly tests passed, but two things were broken:

| KPI | Value | Note |
|---|---|---|
| message order, 20 msgs @ 0/5/10/20% | 20/20 at all four | P0 was already correct |
| loss recovery latency | **52.3 ms** | failed `TestFastRetransmitRecoversWithinOneRTO`: above the 50 ms RTO, so the repair came from a timer |
| 512 KiB @ 0% loss | 128 MiB/s | |
| 512 KiB @ 5% loss | **1.75 MiB/s** | |
| race | clean (one test-harness race found later) | |

Root cause of the two failures was the same: loss detection only ran inside the
ACK handler.

## Round 1 — loss detection on the timer tick

**What changed.** `detectLossLocked` was extracted and also called from the
connection's timer loop, so the reordering window is evaluated even when no ACK
is arriving. A `Send` bursts a whole message, so the last ACK often arrives
before the reordering window has elapsed; after that no ACK comes at all, and
every loss in the burst had to wait out an RTO.

**KPI delta.**

| KPI | before | after |
|---|---|---|
| loss recovery latency | 52.3 ms | **2.4 ms** |
| 512 KiB @ 5% | 1.75 MiB/s | **4.6 MiB/s** |

Recovery latency went from "one RTO" to "one reordering window".

## Round 2 — RTO floor from 50 ms to 5 ms

**What changed.** `MinRTO` 50 ms → 5 ms, with an effective floor of `4*RTTick`
(8 ms). RFC 6298's 1 s floor was written for a path whose only timing source was
a coarse clock; here srtt and rttvar are measured per packet.

**Why.** The path RTT is 40–60 µs. A 50 ms floor is 800× the path delay, and
every loss at the tail of a burst — where no later ACK can trigger fast
retransmit — paid it in full.

**KPI delta.**

| KPI | before | after |
|---|---|---|
| 512 KiB @ 5% | 4.6 MiB/s | **21 MiB/s** |
| 512 KiB @ 10% | 1.9 MiB/s | **12.6 MiB/s** |
| 512 KiB @ 20% | 1.1 MiB/s | **6.6 MiB/s** |
| 512 KiB @ 0% | 128 MiB/s | 105–128 MiB/s (unchanged) |

## Round 3 — pacing debt instead of a per-packet timer

**What changed.** `waitPacing` carries a sub-250 µs deficit instead of sleeping
it off, so a burst costs one timer wakeup rather than one per datagram. The
average rate is unchanged.

**Why.** On loopback the computed slot is tens of microseconds while a timer
wakeup costs ~200 µs, so per-datagram pacing was slower than no pacing at all.

**KPI delta.** 512 KiB @ 5%: 21 → 21 MiB/s (neutral on this path; kept because
it removes a per-packet timer from the hot path on every path).

## Round 4 — ACK bitmap replaced with QUIC-style ranges

**What changed.** The acknowledgement changed from `una + fixed bitmap` to
`una + n × (gap, length)` runs, `maxAckRanges = 16`.

**Why.** The bitmap has to be as wide as the send window to be sound, so a
1024-packet window forced 128 bytes on every datagram — 11% of the MTU, paid on
data *and* on every ACK. Runs cost 5 bytes for the no-loss case and 9–13 bytes
for one or two holes, and still describe the whole window.

**KPI delta.**

| KPI | before | after |
|---|---|---|
| 512 KiB @ 0% | 107 MiB/s | **129–150 MiB/s** |
| 512 KiB @ 5% | 17.9 MiB/s | **27.8 MiB/s** |

(Round 10 later widened the payload again by shrinking the run reservation; the
final table is the authoritative one.)

Two correctness bugs were found while landing this and fixed in the same round:
the run encoder wrote `gap` as one byte instead of two, and the payload offset
was computed from the *encoded* block length instead of the reserved slot, which
silently truncated every fragment.

## Round 5 — loss detection: OR, not AND

**What changed.** Packet threshold (3 later ACKs) and time threshold (9/8 RTT)
are now alternatives, which is what RFC 9002 §6.1 specifies. The code required
both.

**Why.** A four-fragment message that loses one fragment has only three later
packets, so the packet threshold is met exactly at the boundary, and if the ACK
for the third arrives before the reordering window elapses the fragment is never
declared lost. Every repair came from the RTO timer.

**KPI delta.**

| KPI | before | after |
|---|---|---|
| 512 KiB @ 5%, RTO fires | 6 | **0** |
| 512 KiB @ 5%, fast retransmits | 0 | **9** |

## Round 6 — no spurious retransmits at zero loss

**What changed.** The time threshold additionally requires that the packet sits
behind something the peer acknowledged (`!seqAfter(p.seq, largestAcked)`).

**Why.** A burst puts a whole window in flight in far less than a reordering
window, so "older than 9/8 RTT" alone described most of a healthy burst.

**KPI delta.** 512 KiB @ 0%: 25–38 spurious fast retransmits per transfer → **0**.

## Round 7 — batch out-of-order ACKs

**What changed.** Only a completed message or a duplicate forces an immediate
ACK; out-of-order arrivals are batched like in-order ones.

**Why.** A single ACK block already reports every run the receiver knows, so N
immediate ACKs carry the same information as one. A loss makes every subsequent
datagram "out of order", so the receiver was sending one ACK per data datagram.

**KPI delta.**

| KPI | before | after |
|---|---|---|
| ACK datagrams per data datagram @ 5% | 0.98 | **0.13** |
| 512 KiB @ 5% | 30.8 MiB/s | **34.9 MiB/s** |

## Round 8 — the retransmission timer was being overwritten

**What changed.** Three defects in one round, all in the retransmission path:

1. `writeData` reset `p.rto` to the current estimate on *every* transmission,
   undoing the exponential backoff the RTO path had just applied. Now only the
   first transmission stamps it, and `enqueueLocked` stamps it at enqueue time
   (the timer tick could run between enqueue and write, seeing a zero `sentAt`
   and treating the packet as 292 years overdue).
2. Backoff is derived from the current estimate scaled by the attempt count,
   instead of doubling the stored value in place — which never recovers.
3. `timeoutScan` retransmits the *true* queue head. It used to take the first
   expired packet, which after a fast retransmit is often not the head, so the
   head waited for its own already-doubled timer.

**KPI delta.** This was the single largest round.

| KPI | before | after |
|---|---|---|
| 512 KiB @ 5% | 22.6 MiB/s | **80.7 MiB/s** |
| 512 KiB @ 10% | 22.6 MiB/s | **60.7 MiB/s** |
| 512 KiB @ 20% | 11.3 MiB/s | **41.0 MiB/s** |
| spurious RTO retransmits @ 0% | 1–2 per transfer | **0** |

## Round 9 — fast retransmit may repeat, throttled by minRTT

**What changed.** A retransmitted packet may be declared lost again, but not
sooner than `max(4*minRTT, rto/4, 1ms)` after its retransmission. `MaxRetries`
12 → 20.

**Why.** The alternatives were both measured and both worse. A permanent
"already lost" flag hands every second loss of the same datagram to the RTO timer
(measured: a 9-second 512 KiB transfer at 10% loss). Re-evaluating on every ACK
produces one copy per ACK, because the packet threshold is monotone (404
retransmissions for 478 fragments at 20% loss). Throttling on srtt is a feedback
loop — srtt grows under load, and load is when retransmissions happen — which
showed up as 20%-loss transfers varying between 20 ms and 92 ms. minRTT is
queue-free and does not move.

**KPI delta.**

| KPI | before | after |
|---|---|---|
| 512 KiB @ 5% | 50.3 MiB/s | **49.3 MiB/s** (within noise) |
| 512 KiB @ 10% | 41.6 MiB/s | **48.8 MiB/s** |
| 512 KiB @ 20% | 22.8 MiB/s | **24.9 MiB/s** |
| 512 KiB @ 20%, run-to-run spread | 20–92 ms | **20–31 ms** |
| RTO-driven retransmissions, all rates | 5–34 | **0–1** |

## Round 10 — eight ACK runs instead of sixteen

**What changed.** `maxAckRanges` 16 → 8.

**Why.** Each reserved run costs 4 bytes of payload on every data datagram, so
the reservation shrinks every datagram and therefore increases the datagram
count. At 20% loss the extra datagrams are exactly what hurts.

**KPI delta** (dedicated benchmark, 30 iterations each):

| KPI | 16 runs | 8 runs |
|---|---|---|
| 512 KiB @ 5% | 46.8 MB/s | **60.0 MB/s** |
| 512 KiB @ 20% | 5.4 MB/s | **13.8 MB/s** |
| retransmissions @ 20% | 443 | 598 |

The narrower block reports the deep window less precisely, which costs
retransmissions, but never stalls or misorders delivery: the block always states
`una` outright, so the head hole is always reportable.

## Final numbers

| KPI | target | round 0 | final |
|---|---|---|---|
| message order, 20 msgs @ 0/5/10/20% | 100% | 100% | **100%** |
| loss recovery latency | < 1 RTO | 52.3 ms (1.05 RTO) | **490 µs (0.06 RTO)** |
| 512 KiB @ 0% loss | ≥ 100 MiB/s | 128 MiB/s | **145–165 MiB/s** |
| 512 KiB @ 5% loss | — | 1.75 MiB/s | **82–138 MiB/s** |
| 512 KiB @ 10% loss | — | 1.9 MiB/s | **60–61 MiB/s** |
| 512 KiB @ 20% loss | — | 1.1 MiB/s | **28–40 MiB/s** |
| reassembly memory vs forged msgIDs | ≤ RecvWindow | (inherited, correct) | **= RecvWindow** |
| race | zero warnings | 1 (test harness) | **0** |

The final column is three consecutive best-of-11 sweeps on an otherwise idle
machine; the ranges are real spread, not rounding. The 0% number is
syscall-bound — 76% of CPU is in `rawsyscalln`, one `WriteTo` per datagram — and `sendmmsg` batching is the only way past it, which needs a raw fd
and a platform split (see `docs/decisions.md` D55).

## Convergence

Round 10 (eight runs) was the last one with a measurable win. Three further
experiments produced no improvement and were reverted:

- reordering window from `minRTT` instead of `srtt`: 5% went 40.5 → 34.6 MiB/s;
- `AckInterval` sweep 500 µs / 1 ms / 2 ms: all within run-to-run noise;
- a fixed 1 ms floor on the retransmit throttle: 20% went 48 → 68 ms.

The remaining gap to QUIC (~220 MiB/s) is the syscall count per byte, not the
protocol: 76% of CPU is in `rawsyscalln` and the transfer is one `WriteTo` per
datagram. Two consecutive rounds with no improvement were reached, at round 11
and 12 (the three reverted experiments).
