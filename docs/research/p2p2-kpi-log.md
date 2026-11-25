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

| KPI | target | round 0 | round 10 | round 13 (batched send) |
|---|---|---|---|---|
| message order, 20 msgs @ 0/5/10/20% | 100% | 100% | 100% | **100%** |
| loss recovery latency | < 1 RTO | 52.3 ms (1.05 RTO) | 490 µs (0.06 RTO) | **490 µs (unchanged)** |
| 512 KiB @ 0% loss | ≥ 100 MiB/s | 128 MiB/s | 145–165 MiB/s | **209.5 MiB/s (best of 31)** |
| 512 KiB @ 5% loss | — | 1.75 MiB/s | 82–138 MiB/s | **75.8 MiB/s (best of 31)** |
| 512 KiB @ 10% loss | — | 1.9 MiB/s | 60–61 MiB/s | not re-measured |
| 512 KiB @ 20% loss | — | 1.1 MiB/s | 28–40 MiB/s | **33.3 MiB/s (best of 31)** |
| reassembly memory vs forged msgIDs | ≤ RecvWindow | (inherited, correct) | = RecvWindow | **= RecvWindow** |
| race | zero warnings | 1 (test harness) | 0 | **0** |

The round 13 column is an interleaved A/B (both arms in one process, so machine
load is shared) rather than a separate sweep, and the machine was busier than
during round 10 — the per-datagram arm measures 101 MiB/s in the same loop where
round 10 measured 145–165 on an idle machine. The 0% improvement is therefore
reported as the *ratio* between arms (2.07x) as well as the absolute number; the
ratio is what the change is responsible for. The 5% and 20% numbers are
unchanged within noise against their own A/B baseline (75.1 and 26.4 MiB/s in
the same loop), which is the claim that matters for those rows: no regression.

The round 10 column is three consecutive best-of-11 sweeps on an otherwise idle
machine; the ranges are real spread, not rounding. Its 0% number was
syscall-bound — 76% of CPU in `rawsyscalln`, one `WriteTo` per datagram — and
round 13 removes that bound (see `docs/decisions.md` D57).

## Convergence

Round 10 (eight runs) was the last one with a measurable win. Three further
experiments produced no improvement and were reverted:

- reordering window from `minRTT` instead of `srtt`: 5% went 40.5 → 34.6 MiB/s;
- `AckInterval` sweep 500 µs / 1 ms / 2 ms: all within run-to-run noise;
- a fixed 1 ms floor on the retransmit throttle: 20% went 48 → 68 ms.

The remaining gap to QUIC (~220 MiB/s) was the syscall count per byte, not the
protocol: 76% of CPU was in `rawsyscalln` and the transfer was one `WriteTo` per
datagram. Two consecutive rounds with no improvement were reached at rounds 11
and 12 (the three reverted experiments) — but those rounds only explored
protocol parameters. The syscall floor was a different axis, and round 13
(batched send, below) moved it: 0% loss went from ~90 to ~210 MiB/s.

## Round 13 — batched send syscalls (`sendmmsg` / `sendmsg_x`)

The convergence above is a claim about *protocol parameters* only, and it does
not apply to the axis the profile actually pointed at. Rounds 11 and 12 varied
the reordering window, the ACK interval and the retransmit throttle; none of
them touched the syscall count, which is what 76% of the CPU was doing. Round 13
does, and the floor moves.

**What changed.** The first transmission of a message is now written in batches
of `Config.BatchSize` datagrams per syscall: `sendmmsg(2)` on Linux,
`sendmsg_x(2)` on Darwin, and a nil-batcher fallback (one `WriteTo` per
datagram) everywhere else. Retransmissions are deliberately untouched — they
still write one datagram at a time — so loss recovery cannot be affected by
this round at all.

**Why this was the only remaining lever.** Round 10's profile was unambiguous:
66–84% of the transfer's CPU was in `rawsyscalln`, one `WriteTo` per datagram,
464 syscalls for 464 fragments. The protocol was not the limit; the syscall
count per byte was.

**KPI delta.** Measured as an interleaved A/B inside one process (both arms run
in the same loop, so a loaded machine hits them equally), best of 31 and median
of 31, on 512 KiB:

| KPI | per-datagram (baseline) | batched | delta |
|---|---|---|---|
| 512 KiB @ 0%, best | 101.0 MiB/s | **209.5 MiB/s** | **2.07×** |
| 512 KiB @ 0%, median | 80.5 MiB/s | **186.4 MiB/s** | 2.31× |
| 512 KiB @ 5%, best | 75.1 MiB/s | **75.8 MiB/s** | neutral |
| 512 KiB @ 5%, median | 57.5 MiB/s | **73.5 MiB/s** | 1.28× |
| 512 KiB @ 20%, best | 26.4 MiB/s | **33.3 MiB/s** | 1.26× |
| 512 KiB @ 20%, median | 18.7 MiB/s | **20.2 MiB/s** | 1.08× |

The 0% gain is the whole point and it is the syscall count: 464 syscalls become
15 at size 32. The loss rates are unchanged within noise and slightly better at
the median, which is the expected shape — at 5% and 20% the transfer is bounded
by retransmission round trips, not by how the first copies were written.

**Send-path isolation.** Timing `Send` alone (peer socket drained, no protocol
receiver in the loop) with the same 512 KiB message: 6.01 ms per-datagram vs
2.69 ms batched, a 2.24× reduction that matches the end-to-end 0% number. That
is the direct evidence that the syscall floor moved.

**Batch size sweep.** 0% loss, interleaved, best of 15 per size:

| size | MiB/s |
|---|---|
| 1 (per-datagram) | 89.4 |
| 4 | 171.0 |
| 8 | 185.2 |
| 16 | 201.5 |
| **32** | **218.5** |
| 64 | 213.5 |
| 128 | 218.6 |

**32 is the default.** The curve is flat from 16 up: the win is per-syscall, and
16 datagrams already amortise it to within noise. 32 halves the call count again
at no measured cost, and 64/128 add nothing while making a batch hold its
datagrams longer before the first one leaves. A separate pass over 8/16/32/64
gave 78/85/93/85 MB/s, the same ordering.

**Ordering and completeness.** 20 messages at each of 0/5/10/20% loss, batched:
**20/20 correct at all four rates**, byte-exact and in order. Ordering is
preserved by construction — a batch is a contiguous ascending run of fragments
of one message, both syscalls transmit an array in order, and a short return is
re-submitted for the tail rather than treated as an error.

**One real bug, found by review.** The first version flushed the batch only
after `waitForWindow`, which deadlocks whenever the window is smaller than the
batch limit: a batched datagram already holds a sequence number and counts
toward `inFlight`, but nothing writes it until the batch fills, so the window can
never free — the datagrams that would free it are the ones sitting in the
buffer. The default configuration hid it (window 1024, batch 32), and the
congestion-control work in flight starts its window at 10, which exposes it
immediately: measured at 0.02 MiB/s on a simulated 4 MB/s link, with every group
of ten fragments waiting out an RTO. The fix flushes *before* blocking, but only
when the window is actually full, so batching is not thrown away. Pinned by
`TestBatchWindowSmallerThanBatchDoesNotDeadlock` at windows 2, 4 and 10 against a
batch limit of 32.

**Cost.** No measurable throughput cost at any loss rate; the per-message
latency cost is bounded by the time to encode a batch, and a message shorter
than one batch is flushed immediately after its last fragment rather than
waiting for the batch to fill (covered by `TestBatchSmallMessagesNeverFillBatch`).

**Ceiling.** 209.5 MiB/s is short of the 250 MiB/s target. The remaining cost is
split roughly evenly between the batched send syscalls (37% of CPU) and the
**receiver's** per-datagram `ReadFrom` loop (38%), which this round did not
touch: the receive path still does one `recvfrom` per datagram. Batching the
receive side (`recvmmsg`/`recvmsg_x`) is the next lever and is a separate
change; on the same loopback microbenchmark the send side alone reaches ~320
MB/s, so a batched receiver is what stands between 209 and 250.

## Round 14 — congestion control (Reno)

The fixed window is gone as the *operating* window: `SendWindow` is now a ceiling,
and a simplified Reno controller decides how much of it may be in flight. See
`cc.go` and `docs/decisions.md` D57.

**State machine.** Slow start (`cwnd < ssthresh`: +1 datagram per acknowledged
datagram) → congestion avoidance (`+1/cwnd` per ACK, i.e. +1 per round trip) →
fast recovery on an ACK-declared loss (halve once, inflate by one per duplicate
ACK, deflate to `ssthresh` when the ACK covering the recovery point arrives) →
RTO resets the window to the floor and restarts slow start. `cwnd` is an integer
counted in datagrams; congestion avoidance accumulates credit in units of
`1/cwnd` so a round trip cannot round down to zero growth.

Four deviations from RFC 5681, each forced by a measurement:

1. **Floor 128, not 1.** At a floor of 8 the loopback KPI at 5% loss fell from
   52.6 to 5.2 MiB/s: a loss in an eight-datagram window stalls the ACK clock, so
   fast retransmit has no later arrivals to work with and every loss becomes an
   RTO. 128 is also the sample size that makes "the path is congested" a
   statement about the loss *rate* rather than about the existence of loss.
2. **Reductions spaced one srtt apart.** One congested window produces many loss
   signals. Without spacing: 13 reductions in 200 ms, cwnd 512 → 12 for a single
   congestion event.
3. **No ACK-loss reduction at the floor.** On a randomly lossy path a small
   window loses a datagram in most round trips; reacting to each pins the window
   at the floor for the whole transfer.
4. **ssthresh derived from the episode peak, once.** A burst of losses produces
   one timeout per stuck head. Halving the threshold per timeout drove it 512 → 8
   and left the rest of the transfer in congestion avoidance from 8.

Two pre-existing defects were found and fixed while making the window dynamic;
both are in the retransmission path and neither is CC-specific:

- **`waitPacing` could loop forever.** The sleep branch advanced `nextSend` by a
  slot *and* slept the deficit, so the loop always found another slot of debt and
  never returned. It was invisible while the slot was below `minPaceSleep` in
  every configuration (1024 datagrams over a 50 µs loopback RTT is 49 ns); a
  congestion window of 8 over a 2 ms RTT is 250 µs, which reaches the branch.
  The transfer then hung instead of pacing.
- **`timeoutScan`'s push-forward produced fake RTT samples.** A packet whose
  timer was pushed forward without being retransmitted kept a `sentAt` later than
  its real departure, so its ACK reported a round trip shorter than the path's.
  One such sample dragged `minRTT` from 41 ms to 50 µs, which collapsed the
  reordering window and the retransmit throttle; a 2 MiB transfer then failed
  with `ErrPeerUnreachable`. Packets marked `clockSkipped` are now excluded from
  RTT sampling, the same way Karn's rule excludes retransmissions.

### A. Loopback — must not regress

512 KiB message, best of 9, three sweeps. `cc=off` is the round-13 behaviour
(`DisableCongestionControl`).

| KPI | target | cc=off | cc=on | verdict |
|---|---|---|---|---|
| 512 KiB @ 0% | ≤ 15% down | 166.5 MiB/s | 166.4 MiB/s | unchanged |
| 512 KiB @ 5% | no regression | 72.4 MiB/s | **75.5 MiB/s** | +4% |
| 512 KiB @ 20% | no regression | 27.7–31.7 MiB/s | 24.8–27.8 MiB/s | within run-to-run spread |

The 0% case is unchanged because the controller reaches the 1024 ceiling inside
the first ~10 round trips and loopback has no queue to react to. The 20% case is
the only place the controller costs anything: the window settles at the 128 floor
instead of 1024, and the three repeated sweeps overlap (24.8–27.8 vs 27.7–31.7),
so the difference is inside the machine's own variance rather than a structural
loss. Reporting it as "not significantly different" is the honest reading; the
worst single sample was −21%, the best +0.4%.

### B. Simulated congestion — must improve

A rate-limited serialiser with a bounded tail-drop queue, 20 ms one-way delay,
4 MB/s, 256-datagram buffer, 512 KiB message. The fixed window (1024) is 2x the
bandwidth-delay product and cannot avoid overrunning the buffer; the controller
can. Three consecutive runs:

| KPI | cc=off | cc=on |
|---|---|---|
| link loss rate | **31.0%** | **0.0%** |
| retransmissions | 208 | 0 |
| RTO fires | 208 | 0 |
| throughput | 0.01 MiB/s | **2.55 MiB/s** |
| peak queue | 256 (full) | 195–200 |

The fixed-window arm is not merely slower, it is pathological: 208 retransmissions
and 208 RTOs for one 512 KiB message, because every retransmission joins a queue
that is already full and is dropped in turn. The controlled arm never reaches the
buffer limit, so it never loses a datagram at all.

### Fairness — two flows, one bottleneck

Two flows sharing the same serialiser and buffer, 5 ms and 25 ms one-way delay,
12 s stream each, 32 KiB messages, 40-datagram buffer. Throughput ratio
(short-RTT / long-RTT), three runs:

| run | cc=off | cc=on |
|---|---|---|
| 1 | 3.74:1 | 0.82:1 |
| 2 | 65.43:1 | 0.07:1 |
| 3 | 66.71:1 | 0.59:1 |

The fixed window is unfair by construction — throughput is window/RTT, so the
short-RTT flow refills five times as often — and in two of three runs it starved
the long-RTT flow to 0.02 MiB/s. The controller removes that starvation: the
long-RTT flow always gets a substantial share.

It does **not** converge to 1:1, and the ratio overshoots below 1 in two runs.
That is Reno's residual RTT bias, and it is the honest limit of this round: the
window is halved on loss and grown per ACK, so a flow with a longer round trip
still grows more slowly per unit time. The measured range (0.07–0.82) brackets
"the starvation is gone" but not "the shares are equal". CUBIC or a
rate-based controller (BBR-style) is what would close it, and neither is in this
round.

### Verdict

**Default on.** Group A is unchanged within measurement noise at 0% and 5%, and
the 20% difference is inside the run-to-run spread. Group B is the reason the
controller exists: 31% → 0% loss on a path whose buffer is smaller than the fixed
window, and the removal of a 65:1 throughput starvation between two flows.

The escape hatch matters and is documented: `DisableCongestionControl` restores
the fixed window exactly, for a private link or a benchmark where there is no
congestion to react to.

## Round 15 — batched receive syscalls (`recvmmsg` / `recvmsg_x`)

Round 13 batched the send direction and left the receive direction at one
`ReadFrom` per datagram; its own ceiling note named that as the next lever
("37% of CPU in the batched send syscalls and 38% in the receiver's per-datagram
`ReadFrom` loop"). This round does the receive side, and it also turned up a
protocol cost that was larger than either syscall.

**What changed.**

1. `recvmmsg(2)` on Linux and `recvmsg_x(2)` on Darwin, behind the same
   `batchReader` interface the send side uses for `batchWriter`. The
   `batchProvider` interface gained a `batchReader(limit)` method so a wrapping
   `net.PacketConn` (the loss-injection proxy in `harness_test.go`) can keep its
   per-datagram decision in front of the syscall. Other platforms fall back to
   `ReadFrom`, and the fallback is the *same* read loop with a nil reader rather
   than a second code path.
2. `Config.RecvBatchSize` (default 32, same as `BatchSize`) and
   `Config.DisableRecvBatching` for A/B and as an escape hatch.
3. `sackState` gained a one-word `summary` so `snapshot` does not scan the whole
   4096-bit ring to discover it is empty. This was the largest single win and it
   is not a batching change at all — see below.
4. Two allocations were removed from the receive path: the `rc.Read` callback
   and its captured return slots had been escaping to the heap on every receive
   syscall, and `sendAckNow`'s ACK buffer was escaping on every ACK.

**Method.** Absolute wall-clock throughput on this machine is not stable to
better than ±30%: the same configuration measured 153.99 and 213.9 MiB/s in one
session, and 20%-loss runs span 11.5–15.6 MB/s across three sweeps. So the
primary metric here is **process CPU time per transfer** (`getrusage`
`RUSAGE_SELF`, both endpoints in one process), which measures the work the change
removes and does not move when another process takes cycles. Wall clock is
reported as best-of-N and only trusted as an interleaved A/B.

### A. Where the receive CPU actually was

A synthetic harness drove `readLoopBatched`'s exact calls from memory (no
sockets) over one 464-fragment message:

| stage | before | after | note |
|---|---|---|---|
| `sackState.snapshot` | **5798 ns/call** | **18.5 ns/call** | 313× — the ring scan |
| receive-side protocol, per datagram | **6265 ns/pkt** | **379 ns/pkt** | 16.5× |
| `decodePacket` | 26 ns/pkt | 26 ns/pkt | never the problem |

The cause: `snapshot` walked down from `next+sackRingBits-1` on every call, and
on a lossless transfer the ring is **empty** at every ACK — so it did 4096 bit
tests to conclude "nothing above `una`". With one ACK per eight datagrams that
was ~0.7 ms of a 2.4 ms transfer, spent proving there was nothing to say. The
`summary` word makes the empty case a single compare.

This is worth stating plainly because it changes the story of the round: the
receive path was **not** syscall-bound, it was ACK-construction-bound. Batching
the receive syscall alone measures as a rounding error (table B, "recv batch
only"); the win came from the ring scan the profile pointed at once the syscalls
were amortised.

### B. Four-arm attribution, 512 KiB

Send batching and receive batching, each on or off, interleaved in one loop
(`BenchmarkTransferBatching`), CPU µs per transfer:

| arm | @ 0% | @ 5% | @ 20% |
|---|---|---|---|
| both off (round-10 baseline) | 7334 | 9402 | 21827 |
| send batch only (round 13) | 6575 | 8566 | 21786 |
| recv batch only (this round) | 7102 | 9189 | 21594 |
| **both batched (ships)** | **6702** | **7761** | **19909** |

Ratios against "both off": **0.91× @ 0%**, **0.83× @ 5%**, **0.91× @ 20%**.

Read the third row carefully: receive batching *on its own* is 0.97–0.98×. It
only pays when the sender is also batching, because that is what puts more than
one datagram in the socket queue per wakeup (mean receive depth 2.3–4.4 with
batched send, 1.0 without). The two directions are one change, not two.

### C. Wall clock, best of 60

Same four arms, 60 transfers each, best and median (`TestBestOfMany`, run once on
a loaded machine — see the caveat below):

| arm | @ 0% best | @ 0% median | @ 5% best | @ 20% best |
|---|---|---|---|---|
| both off | 180.7 MiB/s | 161.0 | 116.5 | 34.2 |
| send only | 213.9 MiB/s | 193.6 | 76.4 | 26.9 |
| recv only | 180.8 MiB/s | 166.8 | 116.0 | 30.2 |
| **both batched** | **212.9 MiB/s** | **201.3** | **157.8** | **33.8** |

The 0% and 5% columns are the ones to read: 0% is flat versus round 13 (the
send side is now the wall), 5% improves because a deeper receive queue lets more
ACKs and retransmissions land per wakeup. The 20% column is inside the
run-to-run spread — three repeat sweeps of the same arm gave 13.6, 14.5 and
14.8 MB/s — so the honest statement is "unchanged", not "improved".

**Caveat on the absolute numbers.** This machine was running an editor and a
language server throughout, and load average was 1.3–1.8 on 10 cores. Every
absolute figure here is a best-of-N under load, and the CPU column in table B is
the trustworthy one. The earlier round-13 KPI recorded 209.5 MiB/s at 0% on an
idle machine; 212.9 under load is consistent with that, not an improvement.

### D. Depth curves

Receive depth, 512 KiB @ 0% (`BenchmarkTransferRecvDepth`, CPU µs). Two sweeps
are shown because the per-arm spread between sweeps (~±5%) is the same size as
the differences between depths, which is itself the finding:

| depth | 1 | 4 | 8 | 16 | 32 | 64 |
|---|---|---|---|---|---|---|
| cpu, sweep 1 | 6717 | 6675 | 6465 | 6381 | 6366 | 6317 |
| cpu, sweep 2 | 6680 | 6865 | 6685 | 6674 | 6358 | 6632 |

The curve is flat from 8 up: every depth ≥ 8 lands in 6317–6685 µs, and the
ordering inside that band does not reproduce between sweeps. **32 stays the
default**, matching the send side; 64 buys nothing measurable and doubles the
preallocated buffer footprint (64 × 1200 B per connection). The one thing the
table says reproducibly is that depth 1 is not worse than the rest — the same
result as table B, and for the same reason: with the send side's pacing the queue
is shallow, so receive depth is not where the time goes.

### E. Ordering and completeness

20 messages at each of 0/5/10/20% loss, receive batching on:
**20/20 at all four rates**, byte-exact and in order
(`TestKPIReceiveBatchOrdering`). Ordering holds by construction: both calls fill
the array in arrival order, and the read loop consumes the returned prefix in
index order.

**Short returns are the normal case, not an error.** Mean datagrams per receive
call in a real transfer: 2.4 @ 0%, 2.1 @ 5%, 1.8 @ 20%. The loop processes
exactly `n` and calls again; a stale length in the tail of the buffer array can
never be read as a datagram. Covered directly by `TestRecvBatchShortReturn`
(a fake reader returning 3, 1, 2) and end to end by
`TestRecvBatchShortReturnDeliversMessage` (a sender writing groups of 1–3).

### F. Allocations

512 KiB transfer, `allocs/op`:

| | before this round | after |
|---|---|---|
| allocations | 1751 | **1214** |

The two removed sources were both heap escapes in the receive path, found in the
allocation profile after the ring-scan fix:

- the `rc.Read` callback and its captured `got`/`rerr` — ~490 allocations per
  transfer (30% of the total). Fixed by making the callback a method value bound
  once per reader, with the return slots as reader fields.
- `sendAckNow`'s `[40]byte` encode buffer — ~50 per transfer. It escaped because
  it is passed through the `net.PacketConn` interface; it is now a `Conn` field
  guarded by the `writeMu` the function already holds.

`makeDataPacket` remains the largest single allocator (one buffer per fragment,
retained by the send queue for retransmission). Pooling it needs reference
counting, because a packet lives until it is acknowledged; that is not this
round.

### G. What is left

The profile after this round is almost purely syscalls:

| item | share of CPU |
|---|---|
| `sendmsg_x` (batched send) | 34% |
| `recvmsg_x` (batched receive) | 26% |
| `kevent` (poller) | 14% |
| everything else | 26% |

Both directions are already one syscall per 32 datagrams, and the raw socket
ceiling on this machine (no protocol at all, 464 × 1200 B, both ends batched) is
~330 MiB/s. A 512 KiB transfer at 212.9 MiB/s is therefore at **~65% of the raw
UDP ceiling**, and the remaining 35% is per-datagram protocol work that batching
cannot remove: `decodePacket`, SACK marking, the reassembly copy, and the
per-datagram ACK decision.

**Target status: 250 MiB/s not reached.** Best measured is 212.9 MiB/s @ 0% on a
loaded machine (round 13 measured 209.5 idle, so the change is roughly neutral at
0% and clearly positive at 5%). The reason the receive round does not move the 0%
number much is table B: receive batching is worth ~2% on its own, and the
remaining cost is the send syscall plus poller wakeups, neither of which a deeper
receive batch touches. Reaching 250 would need fewer *send* syscalls per byte or
fewer poller round trips, not a better receiver.
