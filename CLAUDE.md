# 🎯 Project Overview
This project is an API-Driven Executor for Kubernetes network bandwidth measurement. It orchestrates `iperf3` tests between all K8s nodes to calculate a full N x N bandwidth matrix.

# 🏗️ Immutable Architecture Rules
When generating, modifying, or refactoring code, you MUST strictly adhere to these constraints:

1. **The "Exec" Pattern ONLY:**
   - NEVER create or delete Kubernetes Jobs/Pods for measurement tasks.
   - You MUST use `client-go/tools/remotecommand` (SPDY Executor) to execute `iperf3 -c ...` directly inside the existing `iperf3` DaemonSet pods.

2. **Concurrency Constraint (Round-Robin):**
   - Network tests must run concurrently but WITHOUT creating network bottlenecks.
   - Always use the Round-Robin pairing (Circle Method) algorithm. 
   - A node can only belong to ONE active pair at any given time. If N is odd, the paired node with "DUMMY" must sit idle.

3. **Strict Asynchronous API:**
   - The REST API endpoint (`POST`) triggering the test MUST NOT block the request.
   - It must return `202 Accepted` with a `task_id`.
   - The actual `remotecommand` executions must run in a background Goroutine.

4. **Barrier Synchronization:**
   - In the background task, use `sync.WaitGroup` to wait for all pairs in a single step to finish capturing stdout.
   - Always enforce a `time.Sleep(5 * time.Second)` cooldown between testing rounds.

# 💻 Golang Coding Standards
- **Standard Library First:** Use `net/http` for the API and `sync` for concurrency. Avoid large frameworks unless explicitly requested.
- **Idiomatic Error Handling:** Wrap errors with context using `fmt.Errorf("failed to [action]: %w", err)`. Do not silently swallow errors.
- **Thread Safety:** Any map holding task states (for the `GET` polling endpoint) MUST be concurrency-safe (e.g., `sync.Map` or struct with `sync.RWMutex`).
- **K8s Client:** Use standard `client-go` in-cluster configuration pattern by default.

# ⚙️ Core Command Context
Whenever generating the exec logic, the base command to be injected into the pod is:
`iperf3 -c <Target_Node_IP> -t 10 --bidir -J`