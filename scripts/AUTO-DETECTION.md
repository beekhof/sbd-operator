# Enhanced Auto-Detection Features

## Cluster Name Auto-Detection 🔍

The setup-shared-storage.sh script now automatically detects the cluster name from your OpenShift/Kubernetes cluster using multiple methods in order of reliability:

### Detection Methods (in priority order):

1. **OpenShift Infrastructure Object** ⭐ *Most Reliable*
   - Uses `infrastructure.cluster/status.infrastructureName`
   - Primary method for OpenShift clusters

2. **OpenShift DNS Configuration** 
   - Extracts cluster name from `dns.cluster/spec.baseDomain`
   - Pattern: `apps.clustername.domain.com` → `clustername`

3. **OpenShift Console Route**
   - Uses console route hostname pattern matching
   - Pattern: `console-openshift-console.apps.clustername.domain.com`

4. **Kubernetes Node Labels**
   - Searches for `kubernetes.io/cluster/*` labels on nodes
   - Works with both OpenShift and EKS clusters

5. **AWS EC2 Instance Tags** (for EKS)
   - Queries EC2 instance tags via AWS API
   - Useful for EKS clusters with proper tagging

6. **kubectl Context Parsing**
   - Intelligent parsing of kubectl context names
   - Multiple pattern recognition algorithms
   - Fallback method when cluster APIs are unavailable

## AWS Region Auto-Detection 🌍

Enhanced region detection using OpenShift/Kubernetes cluster information:

### Detection Methods (in priority order):

1. **OpenShift Infrastructure platformStatus** ⭐ *Most Reliable*
   - Uses `infrastructure.cluster/status.platformStatus.aws.region`
   - Direct AWS region from OpenShift cluster status

2. **Node Provider IDs**
   - Extracts region from AWS provider IDs
   - Pattern: `aws:///us-west-2a/i-1234567890abcdef0` → `us-west-2`

3. **Node DNS Names** 
   - Pattern matching from node hostnames
   - Example: `ip-10-0-1-1.us-west-2.compute.internal`

4. **Node Zone Labels**
   - Uses `topology.kubernetes.io/zone` labels
   - Extracts region from availability zone

5. **OpenShift Machine Config**
   - Uses machine placement.region from Machine API
   - OpenShift-specific deployment information

6. **StorageClass Parameters**
   - Checks EBS CSI StorageClass region parameters
   - Useful when EBS storage is already configured

7. **AWS CLI Configuration**
   - Falls back to local AWS CLI default region

8. **Environment Variables**
   - Uses `AWS_DEFAULT_REGION` if set

## Benefits ✅

- **Zero Configuration**: No need to specify `--cluster-name` or `--aws-region` in most cases
- **OpenShift Native**: Leverages OpenShift-specific APIs and objects
- **Multi-Platform**: Works with OpenShift, EKS, and vanilla Kubernetes
- **Robust Fallbacks**: Multiple detection methods ensure reliability
- **Transparent**: Shows detection method used for troubleshooting
- **Validated**: Automatic cluster name sanitization for AWS compatibility

## Usage Examples

```bash
# Fully automatic - detects everything from cluster
./scripts/setup-shared-storage.sh

# Override auto-detection if needed
./scripts/setup-shared-storage.sh --cluster-name my-cluster --aws-region us-east-1

# See what would be detected
./scripts/setup-shared-storage.sh --dry-run
```

## Error Handling

If auto-detection fails, the script provides:
- Clear error messages listing all attempted methods
- Specific examples showing how to specify values manually
- Helpful debugging information for troubleshooting

This makes the script truly plug-and-play for properly configured OpenShift and Kubernetes clusters! 🚀
