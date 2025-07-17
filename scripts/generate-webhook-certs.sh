#!/bin/bash

# Generate certificates for webhook development
# Supports both Let's Encrypt and self-signed certificates

set -e

CERT_DIR="${CERT_DIR:-/tmp/k8s-webhook-server/serving-certs}"
CERT_NAME="${CERT_NAME:-tls.crt}"
KEY_NAME="${KEY_NAME:-tls.key}"
SERVICE_NAME="${SERVICE_NAME:-webhook-service}"
NAMESPACE="${NAMESPACE:-system}"

# Let's Encrypt configuration
USE_LETSENCRYPT="${USE_LETSENCRYPT:-true}"
WEBHOOK_DOMAIN="${WEBHOOK_DOMAIN:-}"
LETSENCRYPT_EMAIL="${LETSENCRYPT_EMAIL:-}"
LETSENCRYPT_STAGING="${LETSENCRYPT_STAGING:-true}"

echo "Generating certificates for webhook development..."
echo "Certificate directory: $CERT_DIR"
echo "Certificate file: $CERT_NAME"
echo "Key file: $KEY_NAME"
echo "Use Let's Encrypt: $USE_LETSENCRYPT"
if [ "$USE_LETSENCRYPT" = "true" ]; then
    echo "Domain: $WEBHOOK_DOMAIN"
    echo "Email: $LETSENCRYPT_EMAIL"
    echo "Staging: $LETSENCRYPT_STAGING"
fi

# Create certificate directory if it doesn't exist
mkdir -p "$CERT_DIR"

if [ "$USE_LETSENCRYPT" = "true" ]; then
    # Let's Encrypt certificate generation
    if [ -z "$WEBHOOK_DOMAIN" ]; then
        echo "❌ ERROR: WEBHOOK_DOMAIN is required when using Let's Encrypt"
        echo "Example: export WEBHOOK_DOMAIN=sbd-webhook.validatedpatterns.io"
        exit 1
    fi
    
    if [ -z "$LETSENCRYPT_EMAIL" ]; then
        echo "❌ ERROR: LETSENCRYPT_EMAIL is required when using Let's Encrypt"
        echo "Example: export LETSENCRYPT_EMAIL=your-email@example.com"
        exit 1
    fi
    
    # Check if certbot is installed
    if ! command -v certbot &> /dev/null; then
        echo "❌ ERROR: certbot is not installed"
        echo "Please install certbot:"
        echo "  macOS: brew install certbot"
        echo "  Ubuntu/Debian: sudo apt-get install certbot python3-certbot-dns-route53"
        echo "  RHEL/CentOS: sudo yum install certbot python3-certbot-dns-route53"
        exit 1
    fi
    
    # Check if Route53 plugin is available
    if ! certbot plugins | grep -q dns-route53; then
        echo "❌ ERROR: certbot-dns-route53 plugin is not installed"
        echo "Please install the Route53 DNS plugin:"
        echo "  macOS: brew install certbot-dns-route53"
        echo "  Ubuntu/Debian: sudo apt-get install python3-certbot-dns-route53"
        echo "  RHEL/CentOS: sudo yum install python3-certbot-dns-route53"
        echo "  pip: pip install certbot-dns-route53"
        exit 1
    fi
    
    # Check AWS credentials
    if [ -z "$AWS_ACCESS_KEY_ID" ] && [ -z "$AWS_PROFILE" ] && [ ! -f ~/.aws/credentials ]; then
        echo "❌ ERROR: AWS credentials not found"
        echo "Please configure AWS credentials using one of:"
        echo "  1. AWS CLI: aws configure"
        echo "  2. Environment variables: AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY"
        echo "  3. AWS Profile: export AWS_PROFILE=your-profile"
        echo "  4. IAM role (if running on EC2)"
        echo ""
        echo "Required IAM permissions:"
        echo "  - route53:ListHostedZones"
        echo "  - route53:GetChange"
        echo "  - route53:ChangeResourceRecordSets"
        exit 1
    fi
    
    echo "🔄 Obtaining Let's Encrypt certificate for $WEBHOOK_DOMAIN..."
    
    # Determine staging flag
    STAGING_FLAG=""
    if [ "$LETSENCRYPT_STAGING" = "true" ]; then
        STAGING_FLAG="--staging"
        echo "   Using Let's Encrypt staging environment"
    else
        echo "   Using Let's Encrypt production environment"
    fi
    
    # Create certificates directory for certbot
    LETSENCRYPT_DIR="/tmp/letsencrypt"
    mkdir -p "$LETSENCRYPT_DIR"
    
    # Use Route53 DNS plugin for automated challenge
    echo "🔄 Requesting certificate with automated Route53 DNS-01 challenge..."
    echo "   Certificate will be obtained automatically using AWS Route53"
    
    # Set AWS pager to empty to avoid interactive prompts
    export AWS_PAGER=""
    
    certbot certonly \
        --dns-route53 \
        --email "$LETSENCRYPT_EMAIL" \
        --agree-tos \
        --no-eff-email \
        --config-dir "$LETSENCRYPT_DIR/config" \
        --work-dir "$LETSENCRYPT_DIR/work" \
        --logs-dir "$LETSENCRYPT_DIR/logs" \
        $STAGING_FLAG \
        -d "$WEBHOOK_DOMAIN"
    
    # Copy certificates to webhook directory
    CERT_PATH="$LETSENCRYPT_DIR/config/live/$WEBHOOK_DOMAIN"
    if [ ! -f "$CERT_PATH/fullchain.pem" ]; then
        echo "❌ ERROR: Certificate not found at $CERT_PATH/fullchain.pem"
        exit 1
    fi
    
    cp "$CERT_PATH/fullchain.pem" "$CERT_DIR/$CERT_NAME"
    cp "$CERT_PATH/privkey.pem" "$CERT_DIR/$KEY_NAME"
    
    echo "✅ Let's Encrypt certificate obtained successfully!"
    echo "   Certificate: $CERT_DIR/$CERT_NAME"
    echo "   Private key: $CERT_DIR/$KEY_NAME"
    echo "   Domain: $WEBHOOK_DOMAIN"
    echo ""
    echo "📝 Important Notes:"
    echo "   - Certificate expires in 90 days"
    echo "   - You can renew with: certbot renew --config-dir $LETSENCRYPT_DIR/config"
    echo "   - For production, set LETSENCRYPT_STAGING=false"
    echo "   - Your webhook server will listen on port 9443"
    echo ""
    echo "🌐 To test the webhook, ensure your domain points to your development machine:"
    echo "   $WEBHOOK_DOMAIN → $(curl -s ifconfig.me 2>/dev/null || echo 'YOUR_PUBLIC_IP')"
    
else
    # Self-signed certificate generation (fallback)
    echo "🔄 Generating self-signed certificates..."
    
    # Generate private key
    openssl genrsa -out "$CERT_DIR/$KEY_NAME" 2048
    
    # Generate certificate signing request
    cat > "$CERT_DIR/csr.conf" <<EOF
[req]
default_bits = 2048
prompt = no
default_md = sha256
distinguished_name = dn
req_extensions = v3_req

[dn]
C=US
ST=CA
L=San Francisco
O=Development
OU=SBD Operator
CN=$SERVICE_NAME

[v3_req]
basicConstraints = CA:FALSE
keyUsage = nonRepudiation, digitalSignature, keyEncipherment
subjectAltName = @alt_names

[alt_names]
DNS.1 = $SERVICE_NAME
DNS.2 = $SERVICE_NAME.$NAMESPACE
DNS.3 = $SERVICE_NAME.$NAMESPACE.svc
DNS.4 = $SERVICE_NAME.$NAMESPACE.svc.cluster.local
DNS.5 = localhost
IP.1 = 127.0.0.1
IP.2 = ::1
EOF
    
    # Generate certificate signing request
    openssl req -new -key "$CERT_DIR/$KEY_NAME" -out "$CERT_DIR/server.csr" -config "$CERT_DIR/csr.conf"
    
    # Generate self-signed certificate
    openssl x509 -req -in "$CERT_DIR/server.csr" -signkey "$CERT_DIR/$KEY_NAME" -out "$CERT_DIR/$CERT_NAME" -days 365 -extensions v3_req -extfile "$CERT_DIR/csr.conf"
    
    # Clean up temporary files
    rm -f "$CERT_DIR/server.csr" "$CERT_DIR/csr.conf"
    
    echo "✅ Self-signed certificates generated successfully!"
    echo "   Certificate: $CERT_DIR/$CERT_NAME"
    echo "   Private key: $CERT_DIR/$KEY_NAME"
    echo ""
    echo "⚠️  These certificates are for development use only and should not be used in production."
fi

# Set appropriate permissions
chmod 600 "$CERT_DIR/$KEY_NAME"
chmod 644 "$CERT_DIR/$CERT_NAME"

echo ""
echo "🚀 The webhook server can now start with TLS enabled." 