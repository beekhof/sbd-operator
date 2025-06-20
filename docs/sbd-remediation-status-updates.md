# SBDRemediation Controller Status Updates Enhancement

This document describes the enhanced status update functionality implemented in the SBDRemediation controller to provide robust, idempotent, and conflict-resistant status management.

## Overview

The SBDRemediation controller has been enhanced with comprehensive status update capabilities that include:

- **Robust Error Handling**: Proper distinction between retryable and non-retryable errors
- **Retry Logic**: Exponential backoff for transient failures
- **Idempotent Updates**: Avoid unnecessary API calls when status hasn't changed
- **Conflict Resolution**: Handle concurrent updates gracefully
- **Comprehensive Testing**: Integration tests covering all scenarios

## Key Features

### 1. Robust Status Updates (`updateStatusRobust`)

The `updateStatusRobust` method provides comprehensive status update functionality:

```go
func (r *SBDRemediationReconciler) updateStatusRobust(ctx context.Context, 
    sbdRemediation *medik8sv1alpha1.SBDRemediation, 
    phase medik8sv1alpha1.SBDRemediationPhase, 
    message string) (ctrl.Result, error)
```

**Features:**
- **Idempotency**: Skips update if status already matches the desired state
- **Automatic Timestamping**: Updates `LastUpdateTime` on every change
- **Operator Instance Tracking**: Records which operator instance made the update
- **Phase-based Requeuing**: Different requeue behavior based on the target phase
- **Conflict Handling**: Uses retry logic to handle concurrent updates

### 2. Conflict-Resistant Updates (`updateStatusWithRetry`)

The `updateStatusWithRetry` method handles optimistic locking conflicts:

```go
func (r *SBDRemediationReconciler) updateStatusWithRetry(ctx context.Context, 
    sbdRemediation *medik8sv1alpha1.SBDRemediation) error
```

**Features:**
- **Exponential Backoff**: Configurable retry delays with jitter
- **Conflict Detection**: Automatically retries on `IsConflict` errors
- **Fresh Resource Retrieval**: Gets latest version before each retry
- **Transient Error Handling**: Retries on server timeouts and rate limits
- **Permanent Error Detection**: Stops retrying on non-recoverable errors

### 3. Fencing Error Classification (`FencingError`)

Custom error type for comprehensive fencing operation error handling:

```go
type FencingError struct {
    Operation   string
    Underlying  error
    Retryable   bool
    NodeName    string
    NodeID      uint16
}
```

**Error Categories:**
- **Retryable**: Device I/O errors, sync failures, network issues
- **Non-retryable**: Marshaling errors, invalid configuration, permanent failures

### 4. Retry Logic with Backoff (`performFencingWithRetry`)

The fencing operation includes intelligent retry logic:

```go
func (r *SBDRemediationReconciler) performFencingWithRetry(ctx context.Context, 
    sbdRemediation *medik8sv1alpha1.SBDRemediation, 
    targetNodeID uint16) error
```

**Configuration:**
- **Max Attempts**: 3 (configurable via `maxRetryAttempts`)
- **Base Delay**: 5 seconds (configurable via `baseRetryDelay`)
- **Max Delay**: 30 seconds (configurable via `maxRetryDelay`)
- **Smart Backoff**: Exponential increase with maximum cap

## Status Update Workflow

### Success Path

1. **Pending** → Resource created, waiting for processing
2. **FencingInProgress** → Writing fence message to SBD device
3. **FencedSuccessfully** → Fence message written and verified

### Error Handling

1. **Node ID Mapping Error** → `Failed` (non-retryable)
2. **SBD Device Access Error** → Retry up to 3 times → `Failed`
3. **Write/Sync Error** → Retry up to 3 times → `Failed`
4. **Leadership Wait** → `WaitingForLeadership` (requeue every 10s)

### Status Fields Updated

- `Phase`: Current remediation state
- `Message`: Human-readable status description
- `NodeID`: Numeric ID assigned to target node
- `FenceMessageWritten`: Boolean flag for successful writes
- `OperatorInstance`: Instance that handled the remediation
- `LastUpdateTime`: Timestamp of last update

## Configuration

### Retry Configuration

```go
const (
    maxRetryAttempts = 3
    baseRetryDelay   = 5 * time.Second
    maxRetryDelay    = 30 * time.Second
)
```

### Status Update Configuration

```go
const (
    maxStatusUpdateRetries = 5
    statusUpdateDelay     = 100 * time.Millisecond
)
```

## Error Scenarios and Responses

| Error Type | Retryable | Max Attempts | Final Status | Requeue Behavior |
|------------|-----------|--------------|--------------|------------------|
| Node ID mapping | No | 1 | Failed | No requeue |
| SBD device open | Yes | 3 | Failed | No requeue after final failure |
| Write/sync error | Yes | 3 | Failed | No requeue after final failure |
| Marshaling error | No | 1 | Failed | No requeue |
| Status update conflict | Yes | 5 | Varies | Retry with backoff |
| Leadership unavailable | N/A | N/A | WaitingForLeadership | 10s requeue |

## Integration Tests

The enhanced controller includes comprehensive integration tests covering:

### Success Scenarios
- **Complete fencing workflow**: Pending → FencingInProgress → FencedSuccessfully
- **Status field verification**: All status fields properly populated
- **SBD device verification**: Fence message actually written to device

### Error Scenarios
- **Invalid node names**: Proper error handling and status updates
- **Device access failures**: Retry logic and final failure handling
- **Status update idempotency**: Multiple updates with same values

### Leadership Scenarios
- **Leadership availability**: Processing when leader
- **Non-leader behavior**: Proper handling when not leader

### Retry Logic Tests
- **Transient error retry**: Multiple attempts for recoverable errors
- **Non-retryable errors**: Immediate failure for permanent errors
- **Status update conflicts**: Conflict resolution mechanisms

## Usage Examples

### Basic Status Update

```go
result, err := r.updateStatusRobust(ctx, sbdRemediation, 
    medik8sv1alpha1.SBDRemediationPhaseFencingInProgress,
    "Writing fence message to SBD device")
if err != nil {
    return result, err
}
```

### Error Handling with Classification

```go
if err := r.performFencingWithRetry(ctx, sbdRemediation, targetNodeID); err != nil {
    var fencingErr *FencingError
    var message string
    if errors.As(err, &fencingErr) {
        message = fmt.Sprintf("Fencing operation failed: %s", fencingErr.Error())
    } else {
        message = fmt.Sprintf("Fencing operation failed: %v", err)
    }
    
    _, updateErr := r.updateStatusRobust(ctx, sbdRemediation, 
        medik8sv1alpha1.SBDRemediationPhaseFailed, message)
    return ctrl.Result{}, updateErr
}
```

## Best Practices

1. **Always use `updateStatusRobust`** for status updates instead of direct API calls
2. **Classify errors properly** using `FencingError` for better retry behavior
3. **Handle conflicts gracefully** by relying on the built-in retry mechanisms
4. **Monitor requeue behavior** to avoid infinite loops
5. **Use appropriate logging levels** for different scenarios

## Monitoring and Observability

The enhanced controller provides comprehensive logging:

- **Debug Level**: Status update details, conflict retries
- **Info Level**: Phase transitions, successful operations
- **Error Level**: Permanent failures, retry exhaustion

Key log messages to monitor:
- `Status updated successfully`: Successful status transitions
- `Status already up to date, skipping update`: Idempotent behavior
- `Conflict during status update, retrying`: Conflict resolution
- `Fencing operation failed after X attempts`: Retry exhaustion

## Migration Notes

Existing SBDRemediation resources will automatically benefit from the enhanced status update logic. No manual migration is required.

The enhanced controller is backward compatible with existing API structures while providing improved reliability and observability. 