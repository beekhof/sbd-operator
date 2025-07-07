# Webhook Development with Let's Encrypt

This document explains how to set up and use Let's Encrypt certificates for webhook development with the SBD Operator.

## Prerequisites

### 1. Install Required Tools

**macOS:**
```bash
brew install certbot certbot-dns-route53
```

**Ubuntu/Debian:**
```bash
sudo apt-get update
sudo apt-get install certbot python3-certbot-dns-route53
```

**RHEL/CentOS:**
```bash
sudo yum install certbot python3-certbot-dns-route53
```

### 2. Configure AWS Credentials

Since `aws.validatedpatterns.io` DNS is hosted in AWS Route53, you need AWS credentials with the following permissions:

**Required IAM Permissions:**
- `route53:ListHostedZones`
- `route53:GetChange`
- `route53:ChangeResourceRecordSets`

**Configuration options:**
```bash
# Option 1: AWS CLI
aws configure

# Option 2: Environment variables
export AWS_ACCESS_KEY_ID=your-access-key
export AWS_SECRET_ACCESS_KEY=your-secret-key
export AWS_DEFAULT_REGION=us-east-1

# Option 3: AWS Profile
export AWS_PROFILE=your-profile-name
```

### 3. Set Your Email

```bash
export LETSENCRYPT_EMAIL=your-email@example.com
```

## Development Workflow

### Quick Start (Recommended)

For development with staging certificates (recommended for testing):

```bash
# Set your email
export LETSENCRYPT_EMAIL=your-email@example.com

# Start the controller with staging certificates
make run-dev
```

### Production Certificates

For production-grade certificates:

```bash
# Set your email
export LETSENCRYPT_EMAIL=your-email@example.com

# Start the controller with production certificates
make run-prod
```

### Manual Certificate Generation

You can also generate certificates manually:

```bash
# Generate staging certificates
make webhook-certs-staging

# Generate production certificates
make webhook-certs-letsencrypt

# Generate self-signed certificates (fallback)
make webhook-certs-self-signed
```

## Available Make Targets

| Target | Description |
|--------|-------------|
| `make run` | Run controller with Let's Encrypt certificates (default) |
| `make run-dev` | Run controller with staging certificates and leader election disabled |
| `make run-prod` | Run controller with production certificates |
| `make webhook-certs` | Generate certificates (Let's Encrypt by default) |
| `make webhook-certs-letsencrypt` | Generate production Let's Encrypt certificates |
| `make webhook-certs-staging` | Generate staging Let's Encrypt certificates |
| `make webhook-certs-self-signed` | Generate self-signed certificates |
| `make clean-webhook-certs` | Clean up all generated certificates |

## Domain Configuration

The webhook server uses the domain: `sbd-webhook.aws.validatedpatterns.io`

### DNS Setup

You need to create a DNS A record pointing to your development machine:

```bash
# Get your public IP
curl ifconfig.me

# Create DNS record (example using AWS CLI)
aws route53 change-resource-record-sets \
  --hosted-zone-id Z1234567890 \
  --change-batch '{
    "Changes": [{
      "Action": "CREATE",
      "ResourceRecordSet": {
        "Name": "sbd-webhook.aws.validatedpatterns.io",
        "Type": "A",
        "TTL": 300,
        "ResourceRecords": [{"Value": "YOUR_PUBLIC_IP"}]
      }
    }]
  }'
```

### Port Forwarding

If you're behind a router/firewall, you need to forward port 9443:

```bash
# The webhook server listens on port 9443
# Forward external port 9443 to your machine's port 9443
```

## Troubleshooting

### Certificate Generation Issues

**Problem:** AWS credentials not found
```bash
export AWS_ACCESS_KEY_ID=your-key
export AWS_SECRET_ACCESS_KEY=your-secret
```

**Problem:** Route53 plugin not installed
```bash
# macOS
brew install certbot-dns-route53

# Ubuntu/Debian
sudo apt-get install python3-certbot-dns-route53
```

**Problem:** Domain not accessible
- Check DNS resolution: `nslookup sbd-webhook.aws.validatedpatterns.io`
- Check port forwarding: `nc -zv sbd-webhook.aws.validatedpatterns.io 9443`

### Controller Issues

**Problem:** TLS handshake errors
- Ensure certificates are valid: `openssl x509 -in /tmp/k8s-webhook-server/serving-certs/tls.crt -text -noout`
- Check certificate expiration: `openssl x509 -in /tmp/k8s-webhook-server/serving-certs/tls.crt -dates -noout`

**Problem:** Webhook validation fails
- Check webhook configuration: `kubectl get validatingwebhookconfigurations`
- Verify webhook endpoint: `curl -k https://sbd-webhook.aws.validatedpatterns.io:9443/validate-medik8s-medik8s-io-v1alpha1-sbdconfig`

### Certificate Renewal

Let's Encrypt certificates expire after 90 days. To renew:

```bash
# Renew certificates
certbot renew --config-dir /tmp/letsencrypt/config

# Or regenerate
make clean-webhook-certs
make webhook-certs-letsencrypt
```

## Testing

### Test Certificate Generation

```bash
# Test staging certificates
make webhook-certs-staging

# Verify certificate
openssl x509 -in /tmp/k8s-webhook-server/serving-certs/tls.crt -text -noout | grep "CN="
```

### Test Webhook Server

```bash
# Start controller
make run-dev

# In another terminal, test webhook
curl -k https://sbd-webhook.aws.validatedpatterns.io:9443/validate-medik8s-medik8s-io-v1alpha1-sbdconfig
```

## Security Considerations

### Staging vs Production

- **Staging certificates**: Use for development and testing
  - Higher rate limits
  - Marked as "FAKE" in certificate
  - Not trusted by browsers
  
- **Production certificates**: Use for production deployments
  - Lower rate limits (50 certificates per week)
  - Trusted by all browsers
  - Should be used carefully

### Best Practices

1. **Use staging certificates for development**
2. **Rotate certificates before expiration**
3. **Protect private keys** (already done automatically)
4. **Use strong DNS validation**
5. **Monitor certificate expiration**

## Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `USE_LETSENCRYPT` | Use Let's Encrypt certificates | `true` |
| `WEBHOOK_DOMAIN` | Domain for webhook server | `sbd-webhook.aws.validatedpatterns.io` |
| `LETSENCRYPT_EMAIL` | Email for Let's Encrypt registration | Required |
| `LETSENCRYPT_STAGING` | Use staging environment | `true` |
| `CERT_DIR` | Certificate directory | `/tmp/k8s-webhook-server/serving-certs` |
| `AWS_PROFILE` | AWS profile to use | Default profile |

## Support

If you encounter issues:

1. Check the [Troubleshooting](#troubleshooting) section
2. Verify your AWS credentials and permissions
3. Ensure DNS is properly configured
4. Check firewall and port forwarding settings
5. Review the webhook server logs for specific error messages 