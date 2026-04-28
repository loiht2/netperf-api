# Troubleshooting — Internal Dev Notes

This file records specific bugs encountered during development, their root causes, and the definitive fixes. It exists so future maintainers do not lose time rediscovering the same problems.

---

## Bug 1: "Read-only file system" — iperf3 `--bidir` crashes at startup

### Symptom

`iperf3 --bidir` exits immediately with an error like:

```
iperf3: error - unable to create a new stream: Read-only file system
```

The pod appears `Running`, the exec reaches the container, but the test never produces JSON output. The matrix entry for the affected pair contains `{"error": "iperf3 reported: ..."}`.

### Root cause

`iperf3 --bidir` spawns a reverse stream and writes temporary coordination files under `/tmp`. The DaemonSet container runs with `readOnlyRootFilesystem: true` in its `SecurityContext`. With no writable volume mounted at `/tmp`, every attempt to create those files fails at the OS level.

### Fix

Add an `emptyDir` volume to the DaemonSet spec and mount it at `/tmp`:

```yaml
# deploy/daemonset.yaml
spec:
  template:
    spec:
      volumes:
        - name: tmp
          emptyDir: {}
      containers:
        - name: iperf3
          volumeMounts:
            - name: tmp
              mountPath: /tmp
          securityContext:
            readOnlyRootFilesystem: true   # safe — /tmp is now a separate writable mount
```

`emptyDir` is ephemeral (wiped on pod restart), which is exactly what we want. The root filesystem remains read-only and the container security posture is preserved.

---

## Bug 2: "Bad file descriptor" and stream errors in DaemonSet logs

### Symptom

`kubectl logs -n netperf-api daemonset/iperf3-server` shows a continuous stream of errors like:

```
iperf3: error - Bad file descriptor
iperf3: the client has unexpectedly closed the connection
iperf3: error - parameter exchange failed
```

These appear even when no measurement is running. The errors do not always cause a test failure but they pollute logs, make real errors hard to spot, and occasionally corrupt an in-progress measurement.

### Root cause

**Kubernetes `tcpSocket` liveness/readiness probes.** When the probe type is `tcpSocket`, the Kubelet opens a TCP connection to port 5201 and immediately closes it without completing the iperf3 handshake. From iperf3's perspective this looks like a misbehaving client. The half-open/half-closed connection triggers `Bad file descriptor` inside the iperf3 server loop.

A secondary contributor: the initial Go `remotecommand` exec options had `TTY: true` and/or `Stdin: true` set. iperf3 is not a terminal application — allocating a TTY or holding stdin open causes buffering and framing issues in the SPDY stream.

### Fix

**Replace `tcpSocket` probes with `exec` probes** — the exec runs entirely inside the container and never touches the listening socket:

```yaml
# deploy/daemonset.yaml  (and the equivalent struct in the integration test helper)
livenessProbe:
  exec:
    command: ["iperf3", "--version"]
  initialDelaySeconds: 5
  periodSeconds: 10
readinessProbe:
  exec:
    command: ["iperf3", "--version"]
  initialDelaySeconds: 3
  periodSeconds: 5
```

**Set `TTY: false` and `Stdin: false`** in `podExec` (`internal/executor/executor.go`):

```go
Command:   cmd,
Stdin:     false,   // iperf3 reads no stdin
Stdout:    true,
Stderr:    true,
TTY:       false,   // not a terminal; allocating one causes SPDY framing issues
```

---

## Bug 3: Empty matrix `{}` — silent JSON parsing failure

### Symptom

The API returns `status: completed` with a non-empty `nodes` list, but every matrix value is an empty object or zero:

```json
{
  "matrix": {
    "192.168.40.209": {
      "192.168.40.247": {}
    }
  }
}
```

Pod logs confirm iperf3 ran and produced output (non-zero stdout bytes), but the parsed values are all zero.

This bug manifested as **two independent sub-problems** that both had to be fixed.

---

### Sub-problem A: Warning lines before the JSON document

**Root cause:** Certain iperf3 builds (and certain `--bidir` invocations) print one or more plain-text warning lines to stdout *before* the opening `{`:

```
Warning: --bidir option is in use and is an experimental feature with known problems. Use with caution.
{
  "start": { … },
  …
}
```

`json.Unmarshal` on the entire stdout buffer fails because it is not valid JSON. The original code swallowed the error with `continue`, leaving the matrix entry empty.

**Fix:** Before calling `json.Unmarshal`, extract the JSON object by finding the first `{` and last `}`:

```go
// pkg/iperf3/parser.go
func extractJSON(data []byte) ([]byte, error) {
    start := bytes.IndexByte(data, '{')
    end   := bytes.LastIndexByte(data, '}')
    if start < 0 {
        return nil, fmt.Errorf("no JSON object found in iperf3 output")
    }
    if end <= start {
        return nil, fmt.Errorf("malformed iperf3 output: closing '}' not found after '{'")
    }
    return data[start : end+1], nil
}
```

This is called at the top of `Parse()` before any unmarshalling.

---

### Sub-problem B: Mixed-type JSON fields break `map[string]json.Number`

**Root cause:** The `end.sum_sent` (and sibling) objects in iperf3's JSON output contain **mixed types**:

```json
"sum_sent": {
  "bits_per_second": 914346934.09,
  "retransmits":     11,
  "sender":          true
}
```

The original extraction code decoded the sum object into `map[string]json.Number`. Go's `json.Unmarshal` rejects boolean fields (`"sender": true`) when the target type is `json.Number`, causing the **entire `Unmarshal` call to fail silently**. Because the error was not surfaced, all four bandwidth values were returned as 0, producing the empty `{}` entries in the matrix.

**Fix:** Decode the sum object into `map[string]json.RawMessage` (which accepts any JSON value), then extract *only* `bits_per_second` as a `json.Number`:

```go
// pkg/iperf3/parser.go — bpsFromSumField()
var obj map[string]json.RawMessage
if err := json.Unmarshal(raw, &obj); err == nil {
    if bpsRaw, ok := obj["bits_per_second"]; ok {
        var n json.Number
        if err := json.Unmarshal(bpsRaw, &n); err == nil {
            if f, err := n.Float64(); err == nil {
                return f
            }
        }
    }
    return 0
}
```

`json.RawMessage` captures every field as raw bytes without attempting type conversion, so boolean and integer fields no longer poison the decode.

---

### Additional hardening applied at the same time

- **Never `continue` silently on parse error.** Every failure path in `execPair` writes a diagnostic `BandwidthData{Error: "..."}` into BOTH directional matrix cells for the affected pair so callers can see exactly which directed link is broken. In API JSON, errored cells render `"mbps": null`.
- **`json.RawMessage` for the top-level `end` field.** `Output.End` is kept as `json.RawMessage` and decoded lazily by `ParseEnd`. This makes the parser tolerant of missing or differently-typed fields across iperf3 versions, and prevents a missing `sum_sent_bidir_reverse` from zeroing the whole measurement.

> **Note:** an earlier iteration logged raw iperf3 stdout unconditionally for every pair — see Bug 6 below for why that was reverted.

---

## Bug 4: Sequential rounds — 10 s total instead of ~45 s for 3 nodes

### Symptom

With 3 nodes (3 rounds expected), the full measurement completes in ~10 seconds — only one round's worth of iperf3 time. The remaining pairs are missing from the matrix.

### Root cause

The round loop called `return` immediately after the first round when an individual pair failed, instead of continuing to the next round. The `wg.Wait()` barrier was also missing, so the goroutines for pairs within a round were not fully awaited before the loop advanced.

### Fix

1. **`wg.Wait()` barrier per round** — the loop must not advance to the next round until every goroutine in the current round has written its result:

```go
wg.Wait()   // barrier: all pairs in this round must finish before we proceed
```

2. **Never early-return on pair error** — a failed pair writes an error entry into the matrix but the loop continues. Only a context cancellation (`ctx.Err()`) causes a real early return.

3. **Context-aware cooldown** — the inter-round `time.Sleep` was replaced with a `select` so cancellation is still respected during the pause:

```go
select {
case <-time.After(roundCooldown):
case <-ctx.Done():
    return nil, ctx.Err()
}
```

Using a plain `time.Sleep` here broke the `TestE2E_CancelMidFlight` test — sleep is not interruptible by context cancellation, so all rounds completed even after the context was cancelled, and the task ended with `status: completed` instead of `status: canceled`.

---

## Bug 5: Missing Nodes — `client-go` node listing returned 3 of 6

### Symptom

A 6-node cluster consistently produced a measurement matrix containing only 3 nodes, even though all six nodes carried the configured `WORKER_NODE_LABEL` and were `Ready`. The omitted nodes had healthy iperf3 DaemonSet pods running on them.

### Root cause

The original discovery path was **node-centric**: list `Nodes` by label, filter for `NodeReady == True`, then look up the matching iperf3 pod by `Spec.NodeName`. Two problems compounded:

1. **`client-go` node-list races** — between the moment the executor listed nodes and the moment it listed pods, kubelet status updates could land on the API server, but the executor was operating on a stale node-list snapshot. A node that had just transitioned in/out of `Ready` could be filtered out spuriously.
2. **Label drift** — operators sometimes labelled new nodes asynchronously (Cluster API, Karpenter, etc.). The DaemonSet's `tolerations: Exists` made the iperf3 pod schedule everywhere immediately, but the `WORKER_NODE_LABEL` selector excluded those not-yet-labelled nodes — even though their pods were `Running` and `Ready`.

In practice these two effects combined to mask up to half the cluster.

### Fix

Switch to **pod-based, data-plane-aware discovery**. The new `executor.DiscoverNodes` ignores Node objects entirely and instead lists pods that satisfy:

```
Label:           app=iperf3-server   (the DaemonSet selector)
Phase:           Running
PodReady:        True
HostIP:          non-empty
```

The pod's `Status.HostIP` (the node's real IP under `hostNetwork: true`) is used as both the measurement target address and the map key for `pods/exec`. Pod-based discovery is strictly superior because:

- It asks the data plane "which iperf3 servers are actually accepting connections right now?" — the only question that matters for a measurement.
- It eliminates label-drift bugs (a not-yet-labelled node still has its DaemonSet pod and is therefore included).
- It eliminates the node-list / pod-list race entirely (one API call, one set of objects).
- It requires no `nodes` RBAC permission — `pods/list` is sufficient.

Per-pod skip reasons are logged so an operator can immediately see *why* a node is excluded (`phase=Pending`, `PodReady is not True`, `HostIP is empty`, etc.).

---

## Bug 6: Log Pollution — raw iperf3 JSON in every executor log line

### Symptom

After deploying the parser fix from Bug 3, the executor logged the full raw iperf3 `-J` output (~12–15 KB of JSON) for every pair, every round, every measurement. On a 6-node cluster that's 5 rounds × 3 pairs × ~15 KB ≈ **225 KB of JSON per measurement**, repeated whenever a measurement runs. Symptoms downstream included:

- **Fluentd / Vector forwarders OOM-ing** on bursts of multi-line log records.
- **Log-search UIs hanging** when an operator opened a single pod's log tab.
- **Cloud-logging bills increasing** by an order of magnitude on busy clusters.
- **Terminal output unusable** when running tests locally — pasted JSON dwarfed every other line.

### Root cause

While debugging Bug 3 we deliberately added an unconditional raw-stdout dump so we could correlate parsed values against real iperf3 output:

```go
// REMOVED — was logging the full JSON document on the success path too
log.Printf("[executor] pair %s→%s: raw stdout:\n%s", p.Source, p.Target, stdout)
```

Once parsing was provably correct on the success path, this line was no longer load-bearing — but it remained in place by inertia, dumping the full document on every successful exec.

### Fix

**Only log raw stdout when parsing actually fails.** On the success path, emit a single structured one-liner per pair:

```go
log.Printf("[Round %d] Pair %s <-> %s completed. Ingress: %.2f Mbps, ReverseIngress: %.2f Mbps",
    roundIdx+1, pr.Source, pr.Target, pr.IngressMbps, pr.ReverseIngressMbps)
```

On the failure path, dump the raw stdout truncated to **4 KB** (enough to identify the problem, bounded against runaway output):

```go
if parseErr != nil {
    log.Printf("[executor] pair %s→%s: PARSE ERROR — raw stdout (truncated to 4KB):\n%.4096s",
        p.Source, p.Target, stdout)
    ...
}
```

The byte-size envelope log (`stdout=N bytes stderr=M bytes err=...`) is preserved on every pair — it confirms the exec ran and gives a baseline for diagnosing silent failures, without dumping any payload.

**Result:** on a healthy run the executor now produces one structured line per pair (~120 bytes) instead of ~15 KB. Logs are searchable, forwarders are happy, and parse errors still surface the raw payload exactly when an operator needs it.

---

## Bug 7: Silent 0-byte Output — SPDY Stream Severed by Tailscale Mid-Test (History)

> **Note:** Bug 7 documents the root-cause analysis that led to Bug 8's architecture. The fast-retry approach described here has been superseded by the Store-and-Fetch pattern. It is retained for historical reference only.



### Symptom

Intermittently (not on every run, not on every pair), one or more matrix cells contain
`{"mbps": null, "error": "iperf3 produced no output (SPDY stream closed mid-test)"}` even though:

- The source pod is `Running` and `PodReady=True`.
- The target pod is responding to other pairs in the same round.
- No other error appears in the executor logs for that pair — `err=nil` is logged.
- The test eventually appears in the matrix on a subsequent measurement (it is not systematically broken).

### Root cause

Three conditions must coincide for this failure to occur:

1. **Tailscale reconnect event.** Tailscale operates as an in-kernel WireGuard overlay. During a key-rotation or relay-fallback event it briefly tears down and re-establishes the WireGuard tunnel. This severs the underlying TCP session on the **API server ↔ Kubelet leg** of the exec path.

2. **Graceful TCP FIN, not RST.** When the Kubernetes API server detects the Kubelet-side connection has dropped it performs a graceful SPDY teardown — it sends a SPDY stream-close (DATA frame with FIN, or GOAWAY) rather than letting the kernel emit a TCP RST. The `moby/spdystream` library inside `client-go` interprets a clean stream-close as EOF and returns `nil` from `StreamWithContext`. **This is the critical asymmetry**: SPDY stream lifetime does not map onto process success/failure. A graceful stream close and a successful iperf3 exit are indistinguishable from the client side.

3. **`iperf3 -J` buffers all output.** Unlike plain iperf3 (which prints periodic interval lines), `-J` accumulates the full measurement in memory and writes the JSON document to stdout **only at test completion**. If the SPDY stream is torn down 5 seconds into a 10-second test, iperf3 has produced zero bytes to its stdout pipe yet. The iperf3 process may continue running inside the container and eventually write its JSON to the local pipe — but with the stream already closed, the output goes into a void. The executor's `bytes.Buffer` remains empty, and `execPair` receives `("", "", nil)`.

This is why the condition is intermittent: it requires a Tailscale reconnect (not merely latency) that produces a graceful FIN (not an RST), timed within the iperf3 measurement window.

### Fix

**Fast-Retry strategy** in `execPair` (`internal/executor/executor.go`):

| Attempt | `iperf3 -t` | Per-attempt context timeout | Wait before next |
|---------|-------------|----------------------------|-----------------|
| 1       | 10 s        | 15 s                       | —               |
| 2       | 5 s         | 8 s                        | 3 s             |
| 3       | 5 s         | 8 s                        | 3 s             |
| 4       | 5 s         | 8 s                        | 3 s             |

Rationale for the shorter retry duration: by the time a Tailscale reconnect has fired, the tunnel is typically re-established within 1–2 seconds. A 5-second iperf3 run is sufficient to confirm link health while keeping the total worst-case time for one pair under ~50 seconds (well within the round cooldown budget).

Each attempt creates its own `context.WithTimeout` derived from the task-level context, so a context cancellation still propagates immediately. If the outer context is cancelled between attempts, the retry loop exits without delay.

Terminal failure message (all 4 attempts exhausted):

```json
{ "mbps": null, "error": "failed after 4 attempts (network unstable): iperf3 produced no output (SPDY stream closed mid-test)" }
```

Log sequence for a pair that recovers on the third attempt:

```
[executor] pair 10.0.0.1→10.0.0.2 attempt 1/4 (10s): stdout=0 bytes stderr=0 bytes err=<nil>
[Pair 10.0.0.1<->10.0.0.2] Attempt 1/4 (10s) failed: iperf3 produced no output (SPDY stream closed mid-test). Retrying (Attempt 2/4, 5s)...
[executor] pair 10.0.0.1→10.0.0.2 attempt 2/4 (5s): stdout=0 bytes stderr=0 bytes err=<nil>
[Pair 10.0.0.1<->10.0.0.2] Attempt 2/4 (5s) failed: iperf3 produced no output (SPDY stream closed mid-test). Retrying (Attempt 3/4, 5s)...
[executor] pair 10.0.0.1→10.0.0.2 attempt 3/4 (5s): stdout=4821 bytes stderr=0 bytes err=<nil>
[Round 2] 10.0.0.1→10.0.0.2 = 918.40 Mbps  |  10.0.0.2→10.0.0.1 = 904.12 Mbps
```

---

## Bug 8: Architecture Shift — Store-and-Fetch (File-Based Execution)

### Symptom

The fast-retry approach from Bug 7 reduces the failure rate but does not eliminate it. Each retry re-runs iperf3, which races against another Tailscale reconnect. On clusters with frequent key-rotation events the pair can exhaust all 4 attempts and still fail with:

```json
{ "mbps": null, "error": "failed after 4 attempts (network unstable): iperf3 produced no output (SPDY stream closed mid-test)" }
```

Additionally, each retry wastes 5–10 seconds of wall-clock time: even on a single-flap event the pair takes ≥ 18 s (initial 10 s + 3 s wait + 5 s retry) instead of the normal 10 s.

### Root cause (why fast-retry is insufficient)

The fundamental problem is that the **measurement and the result delivery share the same SPDY stream**. The iperf3 JSON payload is streamed in real time back to the executor. A Tailscale flap during the measurement window severs the stream; iperf3's output for that window is lost regardless of how many times we re-run it. Every retry is a new race against the next flap.

### Fix — Store-and-Fetch Architecture

Decouple the iperf3 execution from result delivery using three sequential exec steps:

**Step A — Run & Redirect (fire and wait)**

```sh
# Executed inside the source pod via SPDY exec:
iperf3 -c <target> -p <port> -J -t 10 --bidir > /tmp/iperf_<taskID>_R<round>_<src>_<dst>.json 2>/dev/null
```

The iperf3 output is redirected to a file in `/tmp` (backed by `emptyDir` — see [hard requirement below](#emptydir-tmp-is-a-hard-requirement)). If the SPDY stream drops mid-test, the iperf3 process continues running inside the container; Go simply waits for the remainder of the 11-second minimum window before proceeding.

| Scenario | Behaviour |
|---|---|
| SPDY stream drops at 3 s | `podExec` returns early. Go waits 8 more seconds. iperf3 finishes at ~10 s and writes the file. |
| SPDY stream completes normally | `podExec` returns after ~10 s. Go waits 1 more second (11 s total), then fetches. |
| iperf3 fails fast ("Connection refused") | Shell exits quickly with error JSON in file. Go waits the remaining 11 s (safe but slightly wasteful). |

**Step B — Fetch (with retry)**

```sh
cat /tmp/iperf_<taskID>_R<round>_<src>_<dst>.json
```

This opens a fresh SPDY connection to retrieve the file. If the cat stream itself is dropped (a second Tailscale flap) or the file is not yet ready, the fetch is retried up to 3 more times with a 1-second wait between attempts. Each fetch attempt is capped at 3 seconds.

**Step C — Cleanup (always runs via `defer`)**

```sh
rm -f /tmp/iperf_<taskID>_R<round>_<src>_<dst>.json
```

Uses `context.Background()` (not the task context) so that a context cancellation does not skip the cleanup and leave stale files in `emptyDir`.

### Why this is strictly better

| Property | Fast-Retry | Store-and-Fetch |
|---|---|---|
| Measurement races Tailscale flaps | Yes — each retry is a new race | No — iperf3 completes regardless of stream state |
| Wasted iperf3 time on flap | 5–10 s per retry × up to 3 retries | 0 — the original run completes |
| SPDY-resilient result delivery | No | Yes — cat runs over a fresh connection |
| emptyDir disk usage | None | One JSON file per in-flight pair (≤ 16 KB each; cleaned up on exit) |
| Cancellation latency | Immediate (ctx cancel) | Immediate — cancelled during Step A wait or Step B fetch retry |

### Log sequence for a pair whose Step A stream dropped then Step B succeeded on first fetch

```
[executor] pair 10.0.0.1→10.0.0.2 Step A: elapsed=3.21s err=stream error
[executor] pair 10.0.0.1→10.0.0.2: Step A returned after 3.21s; waiting 7.79s more before fetch
[executor] pair 10.0.0.1→10.0.0.2 Step B attempt 1/4: stdout=13481 bytes err=<nil>
[executor] pair 10.0.0.1→10.0.0.2: cleaned up /tmp/iperf_<taskID>_R2_10.0.0.1_10.0.0.2.json
[Round 2] 10.0.0.1→10.0.0.2 = 921.40 Mbps  |  10.0.0.2→10.0.0.1 = 907.83 Mbps
```

### `emptyDir` `/tmp` is a hard requirement

The DaemonSet pod (and the integration test DaemonSet) **must** mount an `emptyDir` volume at `/tmp`. Without it the `readOnlyRootFilesystem: true` security context prevents Step A from writing the output file, causing every pair to fail with a "read-only file system" error. See also Bug 1.

```yaml
volumes:
  - name: tmp
    emptyDir: {}
containers:
  - name: iperf3
    volumeMounts:
      - name: tmp
        mountPath: /tmp
    securityContext:
      readOnlyRootFilesystem: true  # safe — /tmp is writable via emptyDir
```
