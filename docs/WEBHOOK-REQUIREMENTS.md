# Webhook Requirements for SBD Operator

## ⚠️ CRITICAL SAFETY REQUIREMENT

**The admission webhook MUST be enabled in all production deployments of the SBD operator.**

Disabling the webhook removes critical safety validations that prevent:
- **Data corruption** in SBD devices
- **Split-brain scenarios** in high-availability clusters  
- **Node slot assignment conflicts**

## Why Webhooks Are Mandatory

### SBD Slot Assignment Safety

In SBD (STONITH Block Device) coordination, each node must be assigned a unique slot ID in the shared storage device. The webhook prevents multiple SBDConfigs from having overlapping node selectors, which could cause:

1. **Slot Assignment Conflicts**: Multiple SBDConfigs trying to assign different slot IDs to the same node
2. **Split-Brain Scenarios**: Conflicting remediation decisions from different SBDConfigs  
3. **Data Corruption**: Overlapping writes to SBD device slots
4. **Cluster Instability**: Nodes receiving conflicting SBD instructions

### Validation at Admission Time

The webhook provides immediate validation when SBDConfigs are created or updated:
- **Immediate feedback** via `kubectl apply` rather than checking controller logs
- **Prevents invalid state** from entering the cluster
- **Better user experience** with clear error messages
- **API consistency** through admission-time validation

## Configuration by Environment

### OpenShift (Recommended)

For OpenShift deployments, webhooks are automatically configured with service-ca:

```yaml
# Uses config/openshift-default
resources:
- ../openshift  # Includes webhook with service-ca
```

**Benefits:**
- **Automatic certificate management** via OpenShift service-ca operator
- **No manual certificate setup** required
- **Automatic CA bundle injection** 
- **Certificate rotation** handled automatically

### Kubernetes with cert-manager

For Kubernetes clusters with cert-manager:

```yaml
# Uses config/default  
resources:
- ../webhook
- ../certmanager
```

**Requirements:**
- cert-manager installed in cluster
- DNS management for Let's Encrypt (if using public certs)
- Proper RBAC for certificate management

### Development/Testing

For development environments:

```bash
# Generate self-signed certificates
make webhook-certs-self-signed

# Run with webhooks enabled
make run-dev
```

## Emergency Procedures

### Temporary Webhook Bypass (EMERGENCY ONLY)

If webhook validation is preventing critical cluster operations:

```bash
# Temporarily disable webhook validation
kubectl delete validatingwebhookconfiguration validating-webhook-configuration

# Apply critical changes
kubectl apply -f critical-sbdconfig.yaml

# Re-enable webhook immediately
kubectl apply -f config/webhook/manifests.yaml
```

**⚠️ WARNING**: This leaves the cluster vulnerable to slot assignment conflicts. Re-enable immediately.

### Webhook Troubleshooting

1. **Check webhook pod status:**
   ```bash
   kubectl get pods -n sbd-operator-system
   kubectl logs -n sbd-operator-system deployment/sbd-operator-controller-manager
   ```

2. **Verify webhook configuration:**
   ```bash
   kubectl get validatingwebhookconfiguration
   kubectl describe validatingwebhookconfiguration validating-webhook-configuration
   ```

3. **Check certificate status (OpenShift):**
   ```bash
   kubectl get secret -n sbd-operator-system webhook-server-certs
   kubectl describe secret -n sbd-operator-system webhook-server-certs
   ```

4. **Test webhook endpoint:**
   ```bash
   # Port forward to webhook service
   kubectl port-forward -n sbd-operator-system service/webhook-service 9443:443
   
   # Test endpoint (will return 404 for GET, but confirms TLS)
   curl -k https://localhost:9443/validate-medik8s-medik8s-io-v1alpha1-sbdconfig
   ```

## Validation Examples

### Valid SBDConfigs (Non-overlapping)

```yaml
---
apiVersion: medik8s.medik8s.io/v1alpha1
kind: SBDConfig
metadata:
  name: worker-nodes
spec:
  nodeSelector:
    node-role.kubernetes.io/worker: ""
  # ... other config

---
apiVersion: medik8s.medik8s.io/v1alpha1  
kind: SBDConfig
metadata:
  name: control-plane-nodes
spec:
  nodeSelector:
    node-role.kubernetes.io/control-plane: ""
  # ... other config
```

### Invalid SBDConfigs (Overlapping - Will Be Rejected)

```yaml
---
apiVersion: medik8s.medik8s.io/v1alpha1
kind: SBDConfig
metadata:
  name: config-1
spec:
  nodeSelector:
    zone: "us-west-2a"

---
apiVersion: medik8s.medik8s.io/v1alpha1
kind: SBDConfig  
metadata:
  name: config-2
spec:
  nodeSelector:
    zone: "us-west-2a"  # OVERLAP - same zone selector
```

**Error Message:**
```
error validating data: ValidationError(SBDConfig): 
node selector validation failed: SBDConfig node selector overlaps with existing SBDConfig 'config-1' in namespace 'sbd-operator-system'. 
Each node can only be managed by one SBDConfig to prevent slot assignment conflicts.
```

## Certificate Management

### OpenShift Service-CA (Recommended)

OpenShift automatically manages certificates via service-ca operator:

- **Service annotation**: `service.beta.openshift.io/serving-cert-secret-name: webhook-server-certs`
- **Webhook annotation**: `service.beta.openshift.io/inject-cabundle: "true"`
- **Automatic renewal**: Certificates renewed before expiration
- **No manual intervention** required

### Manual Certificate Management

For environments without automatic certificate management:

1. **Generate certificates** (development):
   ```bash
   make webhook-certs-self-signed
   ```

2. **Create certificate secret**:
   ```bash
   kubectl create secret tls webhook-server-certs \
     --cert=/tmp/k8s-webhook-server/serving-certs/tls.crt \
     --key=/tmp/k8s-webhook-server/serving-certs/tls.key \
     -n sbd-operator-system
   ```

3. **Update webhook CA bundle**:
   ```bash
   # Extract CA from certificate and base64 encode
   CA_BUNDLE=$(openssl x509 -in /tmp/k8s-webhook-server/serving-certs/tls.crt -outform DER | base64 -w 0)
   
   # Update webhook configuration
   kubectl patch validatingwebhookconfiguration validating-webhook-configuration \
     --type='json' \
     -p="[{'op': 'replace', 'path': '/webhooks/0/clientConfig/caBundle', 'value': '$CA_BUNDLE'}]"
   ```

## Security Considerations

### Failure Policy

The webhook uses `failurePolicy: Fail` which means:
- **If webhook is unavailable**: API requests are rejected
- **If webhook returns error**: API requests are rejected  
- **High availability required**: Webhook must be reliable

### Network Policies

When using network policies, ensure webhook communication is allowed:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-webhook-access
spec:
  podSelector:
    matchLabels:
      control-plane: controller-manager
  policyTypes:
  - Ingress
  ingress:
  - from: []  # Allow from kube-apiserver
    ports:
    - protocol: TCP
      port: 9443
```

## Testing Webhook Validation

### Test Node Selector Overlap Prevention

1. **Create first SBDConfig:**
   ```bash
   kubectl apply -f - <<EOF
   apiVersion: medik8s.medik8s.io/v1alpha1
   kind: SBDConfig
   metadata:
     name: test-config-1
     namespace: sbd-operator-system
   spec:
     nodeSelector:
       test-label: "value1"
     sbdWatchdogPath: "/dev/watchdog"
   EOF
   ```

2. **Try to create overlapping SBDConfig (should fail):**
   ```bash
   kubectl apply -f - <<EOF
   apiVersion: medik8s.medik8s.io/v1alpha1
   kind: SBDConfig
   metadata:
     name: test-config-2  
     namespace: sbd-operator-system
   spec:
     nodeSelector:
       test-label: "value1"  # Same selector - should be rejected
     sbdWatchdogPath: "/dev/watchdog"
   EOF
   ```

3. **Expected result**: Second SBDConfig should be rejected with overlap error.

4. **Cleanup:**
   ```bash
   kubectl delete sbdconfig test-config-1 -n sbd-operator-system
   ```

## Deployment Status

- ✅ **Default config**: Webhook enabled with cert-manager support
- ✅ **OpenShift config**: Webhook enabled with service-ca  
- ✅ **Smoke tests**: Webhook enabled for validation testing
- ⚠️ **E2E tests**: Webhook disabled to avoid certificate complexity

## Migration Guide

### Enabling Webhooks in Existing Deployments

If you have an existing deployment without webhooks:

1. **Check current webhook status:**
   ```bash
   kubectl get validatingwebhookconfiguration
   kubectl get pods -n sbd-operator-system
   ```

2. **Update deployment configuration:**
   ```bash
   # For OpenShift
   kubectl apply -k config/openshift-default/
   
   # For Kubernetes with cert-manager  
   kubectl apply -k config/default/
   ```

3. **Verify webhook is working:**
   ```bash
   # Test with invalid SBDConfig (should be rejected)
   kubectl apply -f test/invalid-overlapping-config.yaml
   ```

4. **Monitor logs:**
   ```bash
   kubectl logs -f -n sbd-operator-system deployment/sbd-operator-controller-manager
   ```

Remember: **Webhooks are not optional**. They provide critical safety validations that prevent data corruption and cluster instability in SBD deployments. 