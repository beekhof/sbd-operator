# SBD Agent Prometheus Metrics

The SBD Agent now exposes Prometheus metrics to provide observability into its health, performance, and cluster status.

## Metrics Configuration

### Command Line Flag
- `--metrics-port`: Port for Prometheus metrics endpoint (default: 8080)

### Environment Variables
The metrics port can also be configured through environment variables if needed in the deployment configuration.

## Available Metrics

### Agent Health Metrics

#### `sbd_agent_status_healthy` (Gauge)
- **Description**: Overall SBD Agent health status
- **Values**: 
  - `1` = Agent is healthy (watchdog accessible, SBD device working)
  - `0` = Agent is unhealthy (watchdog failures, SBD device issues)
- **Usage**: Monitor overall agent health for alerting

#### `sbd_watchdog_pets_total` (Counter)
- **Description**: Total number of successful watchdog pets
- **Usage**: Track watchdog activity and detect pet failures

#### `sbd_self_fenced_total` (Counter)
- **Description**: Total number of self-fence operations initiated by this agent
- **Usage**: Monitor critical self-fencing events

### SBD Device Metrics

#### `sbd_device_io_errors_total` (Counter)
- **Description**: Total I/O errors when interacting with the shared SBD device
- **Usage**: Monitor SBD device health and detect storage issues

### Cluster Status Metrics

#### `sbd_peer_status` (GaugeVec)
- **Description**: Current liveness status of peer nodes in the cluster
- **Labels**:
  - `node_id`: Numeric ID of the peer node (1-255)
  - `node_name`: Name of the peer node (e.g., "node-1", "node-2")
  - `status`: Status type ("alive" or "unhealthy")
- **Values**:
  - `1` = Node is in this status
  - `0` = Node is not in this status
- **Usage**: Monitor cluster membership and detect failed nodes

## Metrics Endpoint

The metrics are exposed via HTTP at:
```
http://localhost:8080/metrics
```

### Example Output
```
# HELP sbd_agent_status_healthy SBD Agent health status (1 = healthy, 0 = unhealthy)
# TYPE sbd_agent_status_healthy gauge
sbd_agent_status_healthy 1

# HELP sbd_device_io_errors_total Total number of I/O errors encountered when interacting with the shared SBD device
# TYPE sbd_device_io_errors_total counter
sbd_device_io_errors_total 0

# HELP sbd_watchdog_pets_total Total number of times the local kernel watchdog has been successfully petted
# TYPE sbd_watchdog_pets_total counter
sbd_watchdog_pets_total 1547

# HELP sbd_peer_status Current liveness status of each peer node (1 = alive, 0 = unhealthy/down)
# TYPE sbd_peer_status gauge
sbd_peer_status{node_id="2",node_name="node-2",status="alive"} 1
sbd_peer_status{node_id="2",node_name="node-2",status="unhealthy"} 0
sbd_peer_status{node_id="3",node_name="node-3",status="alive"} 0
sbd_peer_status{node_id="3",node_name="node-3",status="unhealthy"} 1

# HELP sbd_self_fenced_total Total number of times the agent has initiated a self-fence
# TYPE sbd_self_fenced_total counter
sbd_self_fenced_total 0
```

## Prometheus Configuration

### Scrape Configuration
Add this to your Prometheus configuration to scrape SBD Agent metrics:

```yaml
scrape_configs:
  - job_name: 'sbd-agent'
    static_configs:
      - targets: ['node1:8080', 'node2:8080', 'node3:8080']
    scrape_interval: 15s
    metrics_path: /metrics
```

### Kubernetes ServiceMonitor
For Kubernetes deployments with the Prometheus Operator:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: sbd-agent-metrics
  namespace: sbd-system
spec:
  selector:
    matchLabels:
      app: sbd-agent
  endpoints:
  - port: metrics
    interval: 15s
    path: /metrics
```

## Alerting Rules

### Recommended Alerts

```yaml
groups:
- name: sbd-agent
  rules:
  - alert: SBDAgentUnhealthy
    expr: sbd_agent_status_healthy == 0
    for: 30s
    labels:
      severity: critical
    annotations:
      summary: "SBD Agent is unhealthy on {{ $labels.instance }}"
      description: "SBD Agent on {{ $labels.instance }} has been unhealthy for more than 30 seconds"

  - alert: SBDDeviceIOErrors
    expr: increase(sbd_device_io_errors_total[5m]) > 0
    labels:
      severity: warning
    annotations:
      summary: "SBD Device I/O errors detected on {{ $labels.instance }}"
      description: "{{ $value }} I/O errors occurred in the last 5 minutes on {{ $labels.instance }}"

  - alert: SBDPeerNodeDown
    expr: sbd_peer_status{status="unhealthy"} == 1
    for: 1m
    labels:
      severity: warning
    annotations:
      summary: "SBD peer node {{ $labels.node_name }} is unhealthy"
      description: "Peer node {{ $labels.node_name }} (ID: {{ $labels.node_id }}) has been unhealthy for more than 1 minute"

  - alert: SBDSelfFenceEvent
    expr: increase(sbd_self_fenced_total[1m]) > 0
    labels:
      severity: critical
    annotations:
      summary: "SBD Agent initiated self-fence on {{ $labels.instance }}"
      description: "SBD Agent on {{ $labels.instance }} has initiated {{ $value }} self-fence event(s) in the last minute"
```

## Implementation Details

### Metrics Updates
- **Agent Health**: Updated when watchdog pet succeeds/fails and SBD device operations succeed/fail
- **Watchdog Pets**: Incremented on each successful watchdog pet
- **I/O Errors**: Incremented on any SBD device read/write failure
- **Peer Status**: Updated when peer heartbeats are processed and liveness is checked
- **Self-Fence**: Incremented when self-fencing is initiated

### Thread Safety
All metrics are thread-safe and can be updated from multiple goroutines (watchdog loop, SBD device loop, heartbeat loop, peer monitor loop).

### Graceful Shutdown
The metrics HTTP server is gracefully shut down when the SBD Agent stops, with a 5-second timeout. 