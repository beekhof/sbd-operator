# SBD Operator End-to-End (E2E) Tests

This directory contains comprehensive end-to-end tests for the SBD Operator that simulate real failure scenarios to validate the operator's remediation capabilities.

## Overview

The E2E tests go beyond basic functionality validation to test the operator's behavior under realistic failure conditions. These tests use **real AWS infrastructure disruptions** to simulate the types of failures that would trigger SBD remediation in production environments.

## Test Categories

### 1. Basic Configuration Tests
- SBD operator deployment and configuration
- Agent DaemonSet creation and readiness
- Basic cluster topology discovery

### 2. AWS-Based Disruption Tests
- **Network Communication Failures**: Uses AWS Security Groups to block network traffic
- **Storage Access Interruptions**: Uses AWS EBS volume detachment to simulate storage failures
- **Node Recovery Scenarios**: Tests automatic recovery after disruptions are removed

### 3. Resilience Tests
- SBD agent crash and recovery
- Non-fencing failure handling
- Large cluster coordination

## Prerequisites

### Cluster Requirements
- **AWS-based Kubernetes cluster** (EKS, OpenShift on AWS, or self-managed)
- At least 3 worker nodes for safe disruption testing
- Nodes must have AWS provider IDs (format: `aws:///region/instance-id`)

### AWS Requirements
- AWS credentials configured (via environment variables, IAM roles, or AWS CLI)
- Required AWS IAM permissions (see [AWS Permissions](#aws-permissions) section)
- Cluster must be running on AWS EC2 instances

### Software Requirements
- `kubectl` configured to access the cluster
- `ginkgo` test framework
- Go 1.21+ for building tests

## AWS Permissions

The E2E tests require the following AWS IAM permissions:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "ec2:DescribeInstances",
        "ec2:DescribeVolumes", 
        "ec2:DescribeSecurityGroups",
        "ec2:CreateSecurityGroup",
        "ec2:DeleteSecurityGroup",
        "ec2:ModifyInstanceAttribute",
        "ec2:AttachVolume",
        "ec2:DetachVolume",
        "ec2:RevokeSecurityGroupEgress"
      ],
      "Resource": "*"
    }
  ]
}
```

### IAM Policy Example

Create an IAM policy with the above permissions and attach it to:
- The EC2 instance role (if using instance profiles)
- The user/role running the tests (if using AWS credentials)
- The service account (if using IAM roles for service accounts in EKS)

## Running E2E Tests

### Recommended: Complete Deployment Pipeline

```bash
# Run e2e tests with complete deployment and environment setup
make test-e2e
```

This uses the comprehensive `scripts/run-tests.sh` pipeline that:
- **Auto-detects environment** (existing cluster, CRC, Kind)
- **Builds and deploys images** appropriately for the environment
- **Deploys the operator** with proper configuration
- **Generates webhook certificates** automatically
- **Creates test namespaces** with proper security contexts
- **Runs the tests** with ginkgo
- **Cleans up resources** after completion (preserves on failure for debugging)

### Alternative: With Webhooks Enabled

```bash
# Run e2e tests with explicit webhook validation
make test-e2e-with-webhooks
```

This uses the same comprehensive pipeline but emphasizes webhook testing.

### Local/Quick Testing (Assumes Operator Deployed)

If you already have the operator deployed and just want to run the test suite:

```bash
# Run e2e tests locally (operator must be already deployed)
make test-e2e-local
```

**Warning:** This assumes:
- Operator is already deployed and running
- Images are already available in the cluster
- Webhook certificates are configured (if webhooks enabled)
- Test namespaces exist with proper permissions

### Manual Execution with Full Control

```bash
# Use the script directly with custom options
scripts/run-tests.sh --type e2e --env cluster -v

# Or with specific environment
scripts/run-tests.sh --type e2e --env crc -v

# Cleanup only (no tests)
scripts/run-tests.sh --cleanup-only --env cluster

# Build images and run tests
scripts/run-tests.sh --type e2e --env cluster --build -v
```

### Environment Auto-Detection

The test script automatically detects and uses the best available environment:

1. **KUBECONFIG set and cluster accessible** → uses existing cluster
2. **CRC running** → uses CRC (OpenShift local)
3. **Kind cluster exists** → uses Kind
4. **Any cluster accessible** → uses existing cluster
5. **Default** → uses existing cluster for e2e tests

## Webhook Configuration

E2E tests use a **dedicated e2e webhook configuration** to avoid conflicts with OpenShift service-ca operator. This approach ensures reliable webhook testing across different Kubernetes environments.

**Key differences:**
- **OpenShift deployments**: Use service-ca for automatic certificate management
- **E2E tests**: Use dedicated `e2e-webhook-configuration` with self-signed certificates
- **Certificate location**: `/tmp/k8s-webhook-server/serving-certs/` mounted from `webhook-server-certs` secret

**E2E Webhook Architecture:**
- **Webhook Config**: `sbd-operator-e2e-webhook-configuration` (separate from production config)
- **Webhook Service**: `sbd-operator-webhook-service` (dedicated service for e2e tests)
- **Certificate Management**: Manual generation via `scripts/generate-webhook-certs.sh`
- **CA Bundle Injection**: Automatic via `scripts/run-tests.sh` after deployment

This architecture prevents conflicts with:
- OpenShift service-ca operator automatic certificate injection
- Multiple webhook configurations trying to manage the same resources
- Different certificate authorities (service-ca vs self-signed)

## Troubleshooting E2E Tests

### Common Issues and Solutions

**1. RBAC Permission Errors**
```
persistentvolumeclaims is forbidden: User "system:serviceaccount:sbd-operator-system:sbd-operator-sbd-operator-controller-manager" cannot list resource
```

**Root Cause**: Service account name double-prefixing due to kustomize namePrefix configuration.

**Solution**: Fixed by using `serviceAccountName: controller-manager` in manager deployment, allowing kustomize to properly prefix it to `sbd-operator-controller-manager`.

**2. TLS Handshake Errors**
```
TLS handshake error from 10.128.0.31:40814: remote error: tls: bad certificate
```

**Root Cause**: Webhook configuration missing CA bundle for self-signed certificates.

**Solution**: Script automatically updates ValidatingWebhookConfiguration with CA bundle from generated certificate.

**3. OpenShift Service-CA Conflicts**
```
illegal base64 data at input byte 72
```

**Root Cause**: Conflict between OpenShift service-ca operator and manual certificate management.

**Solution**: E2E tests use dedicated `e2e-webhook-configuration` that doesn't conflict with service-ca managed webhooks.

**4. TLS Certificate Name Mismatch**
```
http: TLS handshake error from 10.128.0.31:60266: remote error: tls: bad certificate
```

**Root Cause**: Certificate Subject Alternative Names (SAN) don't match the actual service name.

**Solution**: 
- Certificate generation now uses correct SERVICE_NAME=sbd-operator-webhook-service
- SAN includes all required DNS names: sbd-operator-webhook-service, sbd-operator-webhook-service.sbd-operator-system, etc.
- CN field shortened to avoid 64-character OpenSSL limit

**5. Webhook Certificate Not Found**
```
open /tmp/k8s-webhook-server/serving-certs/tls.crt: no such file or directory
```

**Root Cause**: Mismatch between OpenShift service-ca and generic webhook configurations.

**Solution**: E2E tests use individual kustomize components with dedicated webhook service and configuration instead of conflicting approaches.

## Test Validation Process

### 1. Cluster Validation
The tests automatically validate that:
- The cluster is AWS-based (checks node provider IDs)
- At least 50% of nodes have AWS provider IDs
- Required number of nodes are available for safe testing

### 2. AWS Region Detection
The tests automatically detect the AWS region using:
1. `AWS_REGION` environment variable
2. Node names (e.g., `ip-10-0-1-1.us-west-2.compute.internal`)
3. Node provider IDs (e.g., `aws:///us-west-2a/i-1234567890abcdef0`)

### 3. Permission Validation
Before running disruption tests, the system validates all required AWS permissions by:
- Testing each permission with invalid parameters
- Distinguishing between authorization errors and validation errors
- Failing fast if permissions are insufficient

## Test Scenarios

The e2e tests include several scenarios to validate SBD operator functionality:

### 1. Storage Access Interruption
- **Purpose**: Tests SBD self-fencing when storage becomes unavailable
- **Method**: Detaches non-root EBS volumes from target EC2 instance
- **Validation**: 
  - Node becomes NotReady due to storage issues
  - **Node self-fences automatically when it loses SBD device access**
  - **Node actually panics/reboots (self-fencing verification)**
  - Storage is restored and node recovers
- **Safety**: Only detaches additional volumes, never touches root volume
- **Note**: No SBDRemediation CR needed - node detects storage loss and self-fences

### 2. Network Communication Failure  
- **Purpose**: Tests SBD operator-initiated fencing when kubelet communication is blocked
- **Method**: Creates temporary security group blocking all outbound traffic
- **Validation**:
  - Node becomes NotReady due to kubelet communication failure
  - **Test creates SBDRemediation CR (simulating Node Healthcheck Operator)**
  - SBD remediation is triggered and processed by operator
  - **Node actually panics/reboots (operator-initiated fencing verification)**
  - Network access is restored and node recovers
- **Safety**: Preserves existing security groups, only adds temporary blocking group
- **Note**: SBDRemediation CR required - node has SBD access but needs external trigger

### 3. Other Test Scenarios
- **Basic Configuration**: Tests SBD configuration and agent deployment
- **Agent Crash Recovery**: Tests SBD agent resilience and automatic restart
- **Non-Fencing Failures**: Tests that non-critical issues don't trigger fencing
- **Large Cluster Coordination**: Tests SBD behavior in larger clusters (8+ nodes)

## Test Timing and Expectations

**Important**: The disruption tests now wait for **actual node fencing** (panic/reboot) before cleanup:

- **Expected Duration**: Each disruption test may take 15-20 minutes
- **Timeout Settings**: Tests wait up to 10 minutes for node fencing to occur
- **What You'll See**:
  1. Node becomes NotReady (1-3 minutes)
  2. SBD remediation is created (1-2 minutes)  
  3. **Node panics/reboots due to SBD fencing (5-10 minutes)**
  4. Disruption is removed and node recovers (5-10 minutes)

**This is the correct behavior** - SBD is designed to fence (reboot) unresponsive nodes, and the tests now properly validate this critical functionality.

## SBD Architecture and Component Responsibilities

**Important**: Understanding who creates SBDRemediation CRs is crucial for proper testing:

### Production Architecture:
1. **Node Healthcheck Operator** (or similar external monitoring)
   - Monitors node health and responsiveness
   - Detects when nodes become unhealthy/unresponsive
   - **Creates SBDRemediation CRs** to request fencing

2. **SBD Operator** (this project)
   - Watches for SBDRemediation CRs
   - Processes fencing requests
   - Writes fence messages to shared SBD device
   - Updates SBDRemediation status

3. **SBD Agent** (DaemonSet on each node)
   - Monitors its slot in the SBD device
   - Initiates self-fencing when fence message detected
   - Provides watchdog functionality

### Test Architecture:
The e2e tests validate two different SBD fencing scenarios:

1. **Self-Fencing (Storage Disruption Test)**:
   - Node loses access to SBD device
   - SBD Agent detects storage loss and initiates self-fencing
   - No external intervention required

2. **Operator-Initiated Fencing (Network Disruption Test)**:
   - Node loses network connectivity but retains SBD device access
   - External monitoring (simulated by test) creates SBDRemediation CR
   - SBD Operator processes CR and writes fence message to SBD device
   - SBD Agent detects fence message and initiates fencing

## Test Skipping and Failures

### Automatic Skipping
Tests are automatically skipped when:
- Individual AWS-based tests skip when cluster is not AWS-based or AWS initialization fails
- Insufficient nodes for safe testing
- AWS region cannot be determined
- Required AWS permissions are missing

**Note**: The test suite will run non-AWS tests (like basic configuration and agent crash tests) even when AWS is not available. Only the network and storage disruption tests require AWS.

### Expected Failures
Some test scenarios are designed to trigger failures:
- Node `NotReady` conditions (intentional)
- SBD remediation triggers (expected behavior)
- Temporary resource unavailability (part of test)

## Troubleshooting

### Common Issues

#### 1. "Cluster is not AWS-based" 
```
AWS not available for disruption tests: cluster is not AWS-based, skipping AWS disruption tests
```
**Solution:** This is informational. Non-AWS tests will still run. For AWS disruption tests, ensure you're running on an AWS-based Kubernetes cluster with proper provider IDs.

#### 2. "Failed to detect AWS region"
```
Error: failed to detect AWS region: could not auto-detect AWS region from cluster configuration
```
**Solution:** Set the `AWS_REGION` environment variable or ensure node names contain region information.

#### 3. "AWS permission validation failed"
```
Error: AWS permission validation failed: missing required AWS permissions: ec2:CreateSecurityGroup, ec2:DetachVolume
```
**Solution:** Ensure the IAM role/user has all required permissions listed above.

#### 4. "No suitable non-root volumes found to detach"
```
Skipping storage disruption test: no suitable non-root volumes found to detach
```
**Solution:** This is expected if nodes only have root volumes. The test will skip storage disruption scenarios.

#### 5. Security Group Cleanup Failures
```
Warning: failed to clean up network disruption: failed to delete security group: DependencyViolation
```
**Solution:** The test includes retry logic for this. If manual cleanup is needed:
```bash
# Find and delete the security group manually
aws ec2 describe-security-groups --filters "Name=group-name,Values=sbd-e2e-network-disruptor-*"
aws ec2 delete-security-group --group-id sg-xxxxxxxxx
```

### Debugging

#### 1. Enable Verbose Logging
```bash
ginkgo -v --trace test/e2e
```

#### 2. Check AWS Credentials
```bash
aws sts get-caller-identity
aws ec2 describe-instances --max-items 1
```

#### 3. Verify Node Provider IDs
```bash
kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.providerID}{"\n"}{end}'
```

#### 4. Monitor AWS Resources During Tests
```bash
# Monitor security groups
aws ec2 describe-security-groups --filters "Name=group-name,Values=sbd-e2e-*"

# Monitor volumes
aws ec2 describe-volumes --filters "Name=state,Values=available,in-use"
```

## Safety Considerations

### Production Clusters
**⚠️ WARNING:** These tests perform real infrastructure disruptions. While designed to be safe, they should be used with caution on production clusters.

**Recommendations:**
- Test on dedicated test clusters when possible
- Ensure adequate node redundancy (minimum 3 worker nodes)
- Run during maintenance windows
- Have monitoring in place to detect issues

### Resource Cleanup
The tests include comprehensive cleanup logic:
- `defer` statements ensure cleanup even on test failures
- Automatic restoration of original configurations
- Retry logic for AWS resource cleanup
- Graceful handling of partial failures

### Test Isolation
- Each test uses unique resource names with timestamps
- Tests clean up previous runs before starting
- Temporary AWS resources are clearly tagged
- No persistent changes to cluster configuration

## Environment Variables

- `AWS_REGION`: Override AWS region detection (optional)
- `KUBECONFIG`: Path to Kubernetes configuration file
- `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`: AWS credentials (if not using IAM roles)
- `AWS_PROFILE`: AWS profile to use (alternative to access keys)

## Contributing

When adding new disruption tests:

1. **Follow the safety patterns:**
   - Always use `defer` for cleanup
   - Test with invalid parameters to check permissions
   - Include comprehensive error handling

2. **Add appropriate validation:**
   - Check cluster compatibility
   - Validate required permissions
   - Skip gracefully when prerequisites aren't met

3. **Document the test:**
   - Explain what infrastructure changes are made
   - Document safety measures
   - Include troubleshooting guidance

## Related Documentation

- [Smoke Tests](../smoke/README.md) - Basic functionality validation
- [SBD Protocol Documentation](../../docs/) - Understanding SBD behavior
- [AWS IAM Documentation](https://docs.aws.amazon.com/IAM/) - Managing AWS permissions 