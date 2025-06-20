# sod-operator

## Building and Pushing Images

### Build Images Locally

To build both images locally (without pushing):

```bash
# Build both images with Quay tags
make quay-build

# Build with a specific version
make quay-build VERSION=v1.2.3
```

### Build and Push to Quay Registry

**First, authenticate with the registry:**

```bash
# Login to Quay
docker login quay.io
```

Then build and push:

```bash
# Build and push with default settings (latest tag)
make quay-build-push

# Build and push with a specific version
make quay-build-push VERSION=v1.2.3

# Build and push to a different registry/organization
make quay-build-push QUAY_REGISTRY=your-registry.com QUAY_ORG=your-org VERSION=v1.2.3

# Build and push multi-platform images (linux/amd64, linux/arm64, etc.)
make quay-buildx VERSION=v1.2.3
```

### Authentication

For pushing images, you need to authenticate with the container registry:

```bash
# For Quay.io
docker login quay.io

# For other registries
docker login your-registry.com
```

### Configuration Variables

The following variables can be customized:

- `QUAY_REGISTRY`: Registry URL (default: `quay.io`)
- `QUAY_ORG`: Organization name (default: `medik8s`)
- `VERSION`: Image tag version (default: `latest`)
- `PLATFORMS`: Target platforms for multi-arch builds (default: `linux/arm64,linux/amd64,linux/s390x,linux/ppc64le`)

### Individual Image Building

You can also build images individually:

```bash
# Build operator image only
make docker-build

# Build SBD agent image only
make docker-build-agent

# Build both binaries locally
make build build-agent
```

## Running End-to-End Tests

### Prerequisites for CRC (OpenShift)

The e2e tests now use **CRC (CodeReady Containers)** with OpenShift by default. To run the tests, you need:

1. **Install CRC**: Download from [Red Hat Developers](https://developers.redhat.com/products/codeready-containers/download)
2. **Setup CRC**: Follow the setup guide to configure CRC with appropriate resources
3. **Start CRC**: Ensure CRC is running before running tests

### Running E2E Tests

```bash
# Run e2e tests on CRC OpenShift cluster (default)
make test-e2e

# Explicitly run on CRC OpenShift
make test-e2e-crc

# Run on Kind Kubernetes (legacy support)
make test-e2e-kind

# Setup CRC cluster without running tests
make setup-test-e2e

# Stop CRC cluster after tests
make cleanup-test-e2e
```

### CRC Setup Commands

```bash
# Initial CRC setup (one-time)
crc setup

# Start CRC cluster
crc start

# Check CRC status
crc status

# Stop CRC cluster
crc stop

# Get OpenShift console URL
crc console --url

# Get admin credentials
crc console --credentials
```

### Environment Variables

- `USE_CRC=true`: Force using CRC OpenShift cluster
- `USE_CRC=false`: Force using Kind Kubernetes cluster  
- `CERT_MANAGER_INSTALL_SKIP=true`: Skip CertManager installation
- `TEST_IMG`: Complete override for the test image name (e.g., `quay.io/myorg/sbd-operator:dev`)
- `QUAY_REGISTRY`: Registry URL (defaults to `localhost:5000` for tests, `quay.io` for builds)
- `QUAY_ORG`: Organization/namespace (defaults to `sbd-operator` for tests, `medik8s` for builds)
- `VERSION`: Image version tag (defaults to `e2e-test` for tests, `latest` for builds)

### OpenShift vs Kubernetes Differences

When running on OpenShift (CRC), the tests automatically handle:

- **Security Context Constraints (SCC)** instead of Pod Security Standards
- **OpenShift Routes** for ingress (if applicable)
- **OpenShift Registry** for container images
- **oc** command instead of kubectl where needed

The tests are backward compatible with Kind/Kubernetes for development environments.