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

- **Never `continue` silently on parse error.** Every failure path in `execPair` writes a diagnostic `BandwidthEntry{Error: "..."}` into the matrix so callers can see exactly which pair failed and why.
- **Log raw stdout unconditionally** before parsing. Even when parsing succeeds, the full iperf3 JSON appears in pod logs, which is invaluable when diagnosing field-mapping problems:
  ```go
  log.Printf("[executor] pair %s→%s: raw stdout:\n%s", p.Source, p.Target, stdout)
  ```
- **`json.RawMessage` for the top-level `end` field.** `Output.End` is kept as `json.RawMessage` and decoded lazily by `ParseEnd`. This makes the parser tolerant of missing or differently-typed fields across iperf3 versions, and prevents a missing `sum_sent_bidir_reverse` from zeroing the whole measurement.

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
