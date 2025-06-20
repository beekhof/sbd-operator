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