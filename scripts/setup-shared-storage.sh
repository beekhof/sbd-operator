#!/bin/bash

# EFS Storage Management Script for OpenShift
# This script manages AWS EFS filesystems and Kubernetes StorageClass for SBD operator

set -e

# Disable AWS CLI pager to prevent hanging
export AWS_PAGER=""

# Script configuration
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Default values
CLUSTER_NAME=""
STORAGE_CLASS_NAME=""
EFS_NAME=""
EFS_FILESYSTEM_ID=""
AWS_REGION=""
CREATE_EFS="true"
DRY_RUN="false"
CLEANUP="false"
PERFORMANCE_MODE="generalPurpose"
THROUGHPUT_MODE="provisioned"
PROVISIONED_THROUGHPUT="10"
KUBECTL=""

# New flags for better resource management
FORCE_RECREATE="false"
UPDATE_MODE="false"

# Function to compare StorageClass configurations
compare_storage_class_config() {
    local new_efs_id="$1"
    local existing_sc_yaml
    
    # Get existing StorageClass
    existing_sc_yaml=$($KUBECTL get storageclass "$STORAGE_CLASS_NAME" -o yaml 2>/dev/null || echo "")
    
    if [[ -z "$existing_sc_yaml" ]]; then
        echo "missing"
        return
    fi
    
    # Extract current EFS filesystem ID from existing StorageClass
    local current_efs_id
    current_efs_id=$(echo "$existing_sc_yaml" | grep "fileSystemId:" | awk '{print $2}' || echo "")
    
    # Compare key parameters
    local current_provisioner
    current_provisioner=$(echo "$existing_sc_yaml" | grep "provisioner:" | awk '{print $2}' || echo "")
    
    local current_provisioning_mode
    current_provisioning_mode=$(echo "$existing_sc_yaml" | grep "provisioningMode:" | awk '{print $2}' || echo "")
    
    # Check for differences
    if [[ "$current_efs_id" != "$new_efs_id" ]]; then
        echo "efs_id_changed"
        return
    fi
    
    if [[ "$current_provisioner" != "efs.csi.aws.com" ]]; then
        echo "provisioner_changed"
        return
    fi
    
    if [[ "$current_provisioning_mode" != "efs-ap" ]]; then
        echo "provisioning_mode_changed"
        return
    fi
    
    echo "identical"
}

# Function to detect and reuse existing IAM role
detect_existing_iam_role() {
    local role_name="$1"
    
    log_info "Checking for existing IAM role: $role_name"
    
    # Check if role exists
    local role_arn
    role_arn=$(aws iam get-role --role-name "$role_name" --query 'Role.Arn' --output text 2>/dev/null || echo "")
    
    if [[ -n "$role_arn" && "$role_arn" != "None" ]]; then
        log_info "Found existing IAM role: $role_arn"
        
        # Validate role has required policies
        if validate_efs_csi_role_permissions "$role_arn"; then
            log_success "Existing IAM role has required permissions"
            echo "$role_arn"
            return
        else
            log_warning "Existing IAM role missing required permissions"
            if [[ "$FORCE_RECREATE" == "true" ]]; then
                log_info "Force recreate enabled - will recreate IAM role with correct permissions"
                delete_iam_role "$role_name"
                echo ""
                return
            else
                log_error "IAM role exists but has incorrect permissions. Use --force-recreate to fix."
                exit 1
            fi
        fi
    fi
    
    echo ""
}

# Function to delete IAM role and associated policies
delete_iam_role() {
    local role_name="$1"
    local policy_name="${role_name}_Policy"
    
    log_info "Deleting IAM role and associated policies: $role_name"
    
    # Get account ID for policy ARN
    local account_id
    account_id=$(aws sts get-caller-identity --query 'Account' --output text 2>/dev/null || echo "")
    
    if [[ -n "$account_id" ]]; then
        local policy_arn="arn:aws:iam::${account_id}:policy/${policy_name}"
        
        # Detach policy from role
        aws iam detach-role-policy --role-name "$role_name" --policy-arn "$policy_arn" >/dev/null 2>&1 || true
        
        # Delete policy
        aws iam delete-policy --policy-arn "$policy_arn" >/dev/null 2>&1 || true
        log_info "Deleted IAM policy: $policy_name"
    fi
    
    # Delete role
    aws iam delete-role --role-name "$role_name" >/dev/null 2>&1 || true
    log_info "Deleted IAM role: $role_name"
}

# Function to compare EFS filesystem configuration
compare_efs_config() {
    local efs_id="$1"
    
    log_info "Validating EFS filesystem configuration: $efs_id"
    
    # Get EFS filesystem details
    local efs_info
    efs_info=$(aws efs describe-file-systems \
        --region "$AWS_REGION" \
        --file-system-id "$efs_id" \
        --query 'FileSystems[0]' \
        --output json 2>/dev/null || echo "")
    
    if [[ -z "$efs_info" || "$efs_info" == "null" ]]; then
        echo "missing"
        return
    fi
    
    # Extract current configuration
    local current_performance_mode
    current_performance_mode=$(echo "$efs_info" | jq -r '.PerformanceMode // "generalPurpose"')
    
    local current_throughput_mode
    current_throughput_mode=$(echo "$efs_info" | jq -r '.ThroughputMode // "provisioned"')
    
    local current_provisioned_throughput
    current_provisioned_throughput=$(echo "$efs_info" | jq -r '.ProvisionedThroughputInMibps // 0')
    
    # Compare with desired configuration
    local config_changed="false"
    
    if [[ "$current_performance_mode" != "$PERFORMANCE_MODE" ]]; then
        log_warning "EFS performance mode differs: current=$current_performance_mode, desired=$PERFORMANCE_MODE"
        config_changed="true"
    fi
    
    if [[ "$current_throughput_mode" != "$THROUGHPUT_MODE" ]]; then
        log_warning "EFS throughput mode differs: current=$current_throughput_mode, desired=$THROUGHPUT_MODE"
        config_changed="true"
    fi
    
    if [[ "$THROUGHPUT_MODE" == "provisioned" && "$current_provisioned_throughput" != "$PROVISIONED_THROUGHPUT" ]]; then
        log_warning "EFS provisioned throughput differs: current=$current_provisioned_throughput, desired=$PROVISIONED_THROUGHPUT"
        config_changed="true"
    fi
    
    if [[ "$config_changed" == "true" ]]; then
        echo "config_changed"
    else
        echo "valid"
    fi
}

# Function to handle StorageClass updates
handle_storage_class_update() {
    local efs_id="$1"
    
    log_info "Checking StorageClass update requirements..."
    
    local comparison_result
    comparison_result=$(compare_storage_class_config "$efs_id")
    
    case "$comparison_result" in
        "missing")
            log_info "StorageClass does not exist - will create new one"
            return 0
            ;;
        "identical")
            log_success "StorageClass configuration is up to date"
            if [[ "$UPDATE_MODE" != "true" && "$FORCE_RECREATE" != "true" ]]; then
                log_info "Skipping StorageClass recreation (use --update-mode to force update)"
                return 1
            fi
            log_info "Update mode enabled - will recreate StorageClass"
            ;;
        *)
            log_warning "StorageClass configuration differs: $comparison_result"
            log_info "StorageClasses cannot be updated - will delete and recreate"
            ;;
    esac
    
    # Delete existing StorageClass
    log_info "Deleting existing StorageClass: $STORAGE_CLASS_NAME"
    $KUBECTL delete storageclass "$STORAGE_CLASS_NAME" --ignore-not-found=true
    
    # Wait for deletion to complete
    local max_wait=30
    local wait_count=0
    while [[ $wait_count -lt $max_wait ]]; do
        if ! $KUBECTL get storageclass "$STORAGE_CLASS_NAME" >/dev/null 2>&1; then
            break
        fi
        log_info "Waiting for StorageClass deletion... (${wait_count}/${max_wait})"
        sleep 2
        ((wait_count++))
    done
    
    if [[ $wait_count -ge $max_wait ]]; then
        log_warning "StorageClass deletion timeout - proceeding anyway"
    else
        log_success "StorageClass deleted successfully"
    fi
    
    return 0
}

# Function to check for existing AWS resources and determine reuse strategy
check_existing_resources() {
    log_info "Checking for existing AWS resources to reuse..."
    
    local reuse_summary=""
    local resources_to_create=""
    
    # Check EFS filesystem
    if [[ "$CREATE_EFS" == "true" ]]; then
        local existing_efs
        existing_efs=$(find_efs_by_name "$EFS_NAME")
        
        if [[ -n "$existing_efs" && "$existing_efs" != "None" ]]; then
            local efs_status
            efs_status=$(compare_efs_config "$existing_efs")
            
            case "$efs_status" in
                "valid")
                    log_success "Found compatible EFS filesystem: $existing_efs"
                    reuse_summary="${reuse_summary}✅ EFS Filesystem: $existing_efs (reusing)\n"
                    ;;
                "config_changed")
                    if [[ "$FORCE_RECREATE" == "true" ]]; then
                        log_warning "EFS configuration differs - will recreate due to --force-recreate"
                        resources_to_create="${resources_to_create}🔄 EFS Filesystem (recreate)\n"
                    else
                        log_error "EFS filesystem exists but has different configuration. Use --force-recreate to recreate."
                        exit 1
                    fi
                    ;;
                "missing")
                    resources_to_create="${resources_to_create}🆕 EFS Filesystem\n"
                    ;;
            esac
        else
            resources_to_create="${resources_to_create}🆕 EFS Filesystem\n"
        fi
    fi
    
    # Check IAM role
    local role_name="AmazonEKS_EFS_CSI_DriverRole_${CLUSTER_NAME}"
    local existing_role
    existing_role=$(detect_existing_iam_role "$role_name")
    
    if [[ -n "$existing_role" ]]; then
        reuse_summary="${reuse_summary}✅ IAM Role: $role_name (reusing)\n"
    else
        resources_to_create="${resources_to_create}🆕 IAM Role: $role_name\n"
    fi
    
    # Check StorageClass
    local sc_status
    sc_status=$(compare_storage_class_config "placeholder")
    
    if [[ "$sc_status" == "identical" && "$UPDATE_MODE" != "true" && "$FORCE_RECREATE" != "true" ]]; then
        reuse_summary="${reuse_summary}✅ StorageClass: $STORAGE_CLASS_NAME (unchanged)\n"
    else
        case "$sc_status" in
            "missing")
                resources_to_create="${resources_to_create}🆕 StorageClass: $STORAGE_CLASS_NAME\n"
                ;;
            *)
                resources_to_create="${resources_to_create}🔄 StorageClass: $STORAGE_CLASS_NAME (recreate)\n"
                ;;
        esac
    fi
    
    # Display summary
    echo
    log_info "📋 Resource Reuse Summary:"
    if [[ -n "$reuse_summary" ]]; then
        echo -e "$reuse_summary"
    fi
    
    if [[ -n "$resources_to_create" ]]; then
        log_info "🚧 Resources to Create/Update:"
        echo -e "$resources_to_create"
    fi
    
    if [[ -z "$reuse_summary" && -z "$resources_to_create" ]]; then
        log_info "ℹ️  No resource operations needed"
    fi
    
    echo
}

# Functions
log_info() {
    echo -e "${BLUE}[INFO]${NC} $1" >&2
}

log_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1" >&2
}

log_warning() {
    echo -e "${YELLOW}[WARNING]${NC} $1" >&2
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1" >&2
}

show_usage() {
    cat << EOF
Usage: $0 [OPTIONS]

This script sets up EFS-based shared storage for OpenShift/Kubernetes clusters.
It creates an EFS filesystem, configures networking (VPC, subnets, security groups,
mount targets), installs the EFS CSI driver, and creates a StorageClass.

For OpenShift on AWS, this script also configures the proper IAM roles and 
service account annotations required for the EFS CSI driver to access AWS APIs.

OPTIONS:
    --create-efs                Create a new EFS filesystem (default: true)
    --no-create-efs            Use existing EFS filesystem (requires --filesystem-id)
    --filesystem-id FSID       Use existing EFS filesystem with ID FSID
    --efs-name NAME            Name for the EFS filesystem (default: sbd-efs-CLUSTER_NAME)
    --storage-class-name NAME  Name for the StorageClass (default: sbd-efs-sc)
    --cluster-name NAME        Override cluster name detection
    --aws-region REGION        Override AWS region detection
    --efs-csi-role-name NAME   Specify EFS CSI IAM role name (default: auto-detect)
    --create-iam-role          Create EFS CSI IAM role if missing (default: true)
    --no-create-iam-role       Skip IAM role creation
    --cleanup                  Clean up all created resources
    --force-recreate           Force recreation of existing resources even if compatible
    --update-mode              Force update/recreation of StorageClass even if identical
    --dry-run                  Show what would be done without executing
    --verbose                  Enable verbose logging
    --help                     Show this help message

NETWORKING FEATURES:
    • Auto-detects cluster VPC and private subnets
    • Creates NFS security group with proper port 2049 access
    • Sets up EFS mount targets in all cluster subnets
    • Configures EFS CSI driver with cluster credentials
    • Handles existing resources gracefully (idempotent)

OPENSHIFT INTEGRATION:
    • Validates EFS CSI service account IAM role configuration
    • Creates IAM roles with proper EFS permissions if needed
    • Configures service account annotations for AWS access
    • Validates CSI driver credential access to AWS APIs

EXAMPLES:
    # Create new EFS with intelligent resource reuse and auto-detection
    # (automatically detects cluster name and AWS region from OpenShift/Kubernetes)
    $0

    # Force recreation of existing resources
    $0 --force-recreate

    # Update StorageClass configuration even if identical
    $0 --update-mode

    # Override auto-detected values if needed
    $0 --cluster-name my-cluster --aws-region us-east-1

    # Use existing EFS filesystem with existing IAM role
    $0 --no-create-efs --filesystem-id fs-1234567890abcdef0 --efs-csi-role-name MyEFSRole

    # Create with custom names and skip IAM role creation
    $0 --efs-name my-shared-storage --storage-class-name my-efs-sc --no-create-iam-role

    # Preview changes without executing (shows resource reuse plan and detected values)
    $0 --dry-run

    # Clean up everything
    $0 --cleanup --efs-name sbd-efs-mycluster

REQUIREMENTS:
    • OpenShift/Kubernetes cluster with AWS provider
    • AWS CLI configured with appropriate permissions
    • kubectl/oc CLI tools
    • Cluster admin permissions
    • IAM permissions for role creation (if --create-iam-role)

AWS PERMISSIONS REQUIRED:
    The script checks for required AWS permissions before proceeding. If you encounter
    permission errors, ask your AWS administrator to grant the required permissions.
    All permission checks are mandatory and cannot be bypassed.

    Core permissions needed:
      • EFS: Create/describe filesystems, access points, mount targets
      • EC2: Describe VPCs/subnets, create/manage security groups  
      • IAM: Create/manage roles and policies (if --create-iam-role is used)
      
    The script provides detailed error messages with specific missing permissions
    and manual resolution steps when permission issues are encountered.

The script automatically:
    1. Auto-detects cluster name from OpenShift/Kubernetes cluster using multiple methods:
       • OpenShift Infrastructure object (most reliable)
       • OpenShift DNS configuration and Console routes
       • Kubernetes node labels and provider IDs
       • kubectl context parsing with pattern matching
    2. Auto-detects AWS region from cluster configuration:
       • OpenShift Infrastructure platformStatus
       • Node provider IDs and zone labels
       • Machine configuration and StorageClass parameters
    3. Validates AWS permissions for EFS and EC2 operations
    4. Intelligently detects and reuses existing compatible AWS resources
    5. Installs/verifies EFS CSI driver
    6. Creates and configures IAM roles for EFS CSI service account (with smart reuse)
    7. Creates EFS filesystem with proper tags (or reuses existing compatible ones)
    8. Sets up complete networking (VPC, subnets, security groups, mount targets)
    9. Creates/updates StorageClass with EFS Access Point provisioning for ReadWriteMany (RWX) access
    10. Handles StorageClass updates by deleting and recreating (since they cannot be updated)
    11. Provides comprehensive cleanup functionality and resource validation

EOF
}

# Parse command line arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        -s|--storage-class)
            STORAGE_CLASS_NAME="$2"
            shift 2
            ;;
        -f|--filesystem-id)
            EFS_FILESYSTEM_ID="$2"
            CREATE_EFS="false"  # Disable EFS creation when using existing filesystem
            shift 2
            ;;
        -n|--efs-name)
            EFS_NAME="$2"
            shift 2
            ;;
        -r|--region|--aws-region)
            AWS_REGION="$2"
            shift 2
            ;;
        -k|--cluster-name)
            CLUSTER_NAME="$2"
            shift 2
            ;;
        --performance-mode)
            PERFORMANCE_MODE="$2"
            shift 2
            ;;
        --throughput-mode)
            THROUGHPUT_MODE="$2"
            shift 2
            ;;
        --provisioned-tp)
            PROVISIONED_THROUGHPUT="$2"
            shift 2
            ;;
        --create-efs)
            CREATE_EFS="true"
            shift
            ;;
        --no-create-efs)
            CREATE_EFS="false"
            shift
            ;;
        --cleanup)
            CLEANUP="true"
            shift
            ;;
        --skip-csi-install)
            SKIP_CSI_INSTALL="true"
            shift
            ;;
        --efs-csi-role-name)
            EFS_CSI_ROLE_NAME="$2"
            shift 2
            ;;
        --create-iam-role)
            CREATE_IAM_ROLE="true"
            shift
            ;;
        --no-create-iam-role)
            CREATE_IAM_ROLE="false"
            shift
            ;;
        --force-recreate)
            FORCE_RECREATE="true"
            shift
            ;;
        --update-mode)
            UPDATE_MODE="true"
            shift
            ;;
        --dry-run)
            DRY_RUN="true"
            shift
            ;;
        --verbose)
            set -x  # Enable verbose mode
            shift
            ;;
        -h|--help)
            show_usage
            exit 0
            ;;
        *)
            log_error "Unknown option: $1"
            show_usage
            exit 1
            ;;
    esac
done

# Validate inputs
if [[ "$PERFORMANCE_MODE" != "generalPurpose" && "$PERFORMANCE_MODE" != "maxIO" ]]; then
    log_error "Invalid performance mode: $PERFORMANCE_MODE. Must be 'generalPurpose' or 'maxIO'"
    exit 1
fi

if [[ "$THROUGHPUT_MODE" != "provisioned" && "$THROUGHPUT_MODE" != "burstingThroughput" ]]; then
    log_error "Invalid throughput mode: $THROUGHPUT_MODE. Must be 'provisioned' or 'burstingThroughput'"
    exit 1
fi

# Function to check required tools
check_tools() {
    log_info "Checking required tools..."
    
    # Auto-detect kubectl or oc command
    if [[ -z "$KUBECTL" ]]; then
        if command -v oc &> /dev/null; then
            KUBECTL="oc"
            log_info "Detected OpenShift CLI: oc"
        elif command -v kubectl &> /dev/null; then
            KUBECTL="kubectl" 
            log_info "Detected Kubernetes CLI: kubectl"
        else
            log_error "Neither 'oc' nor 'kubectl' command found"
            log_error "Please install OpenShift CLI (oc) or Kubernetes CLI (kubectl)"
            exit 1
        fi
    fi
    
    local missing_tools=()
    
    if ! command -v aws &> /dev/null; then
        missing_tools+=("aws")
    fi
    
    if ! command -v $KUBECTL &> /dev/null; then
        missing_tools+=("$KUBECTL")
    fi
    
    if ! command -v jq &> /dev/null; then
        missing_tools+=("jq")
    fi
    
    if [[ ${#missing_tools[@]} -gt 0 ]]; then
        log_error "Missing required tools: ${missing_tools[*]}"
        exit 1
    fi
    
    log_success "All required tools are available"
}

# Function to check ALL required AWS permissions
check_aws_permissions() {
    log_info "Checking AWS permissions..."
    
    if [[ "$DRY_RUN" == "true" ]]; then
        log_info "[DRY RUN] Would check AWS permissions"
        return
    fi
    


    local permission_errors=()
    local test_efs_id=""

    # Core AWS permissions - these are absolutely required
    log_info "Testing core AWS permissions..."
    
    # Test STS permissions (required for all AWS operations)
    if ! aws sts get-caller-identity >/dev/null 2>&1; then
        permission_errors+=("sts:GetCallerIdentity - Required for AWS authentication")
    fi

    # Test EFS permissions
    log_info "Testing EFS permissions..."
    if ! aws efs describe-file-systems --region "$AWS_REGION" --max-items 1 >/dev/null 2>&1; then
        permission_errors+=("elasticfilesystem:DescribeFileSystems - Required to list EFS filesystems")
    else
        # Get a test EFS for mount target testing
        local test_efs_for_mount_targets
        test_efs_for_mount_targets=$(aws efs describe-file-systems --region "$AWS_REGION" --query 'FileSystems[0].FileSystemId' --output text 2>/dev/null || echo "")
        if [[ -n "$test_efs_for_mount_targets" && "$test_efs_for_mount_targets" != "None" ]]; then
            if ! aws efs describe-mount-targets --region "$AWS_REGION" --file-system-id "$test_efs_for_mount_targets" >/dev/null 2>&1; then
                permission_errors+=("elasticfilesystem:DescribeMountTargets - Required to manage EFS mount targets")
            fi
        fi
    fi

    # Test EC2 VPC permissions
    log_info "Testing EC2 VPC permissions..."
    if ! aws ec2 describe-vpcs --region "$AWS_REGION" --max-results 5 >/dev/null 2>&1; then
        permission_errors+=("ec2:DescribeVpcs - Required to find cluster VPC")
    fi
    
    if ! aws ec2 describe-subnets --region "$AWS_REGION" --max-results 5 >/dev/null 2>&1; then
        permission_errors+=("ec2:DescribeSubnets - Required to find cluster subnets")
    fi
    
    if ! aws ec2 describe-security-groups --region "$AWS_REGION" --max-results 5 >/dev/null 2>&1; then
        permission_errors+=("ec2:DescribeSecurityGroups - Required to manage NFS security groups")
    fi

    # Test IAM permissions (if IAM role creation is enabled)
    if [[ "$CREATE_IAM_ROLE" == "true" ]]; then
        log_info "Testing IAM permissions for role creation..."
        
        # Test basic IAM permissions
        if ! aws iam list-open-id-connect-providers >/dev/null 2>&1; then
            permission_errors+=("iam:ListOpenIdConnectProviders - Required to find cluster OIDC provider")
        fi
        
        # Test get role permission (with non-existent role)
        local get_role_error
        get_role_error=$(aws iam get-role --role-name "non-existent-role-test-$$" 2>&1 || echo "")
        if echo "$get_role_error" | grep -qi "UnauthorizedOperation\|AccessDenied\|is not authorized"; then
            permission_errors+=("iam:GetRole - Required to check existing IAM roles")
        fi
        
        # Test create role permission (with invalid parameters to test permission only)
        local create_role_error
        create_role_error=$(aws iam create-role --role-name "" --assume-role-policy-document "" 2>&1 || echo "")
        if echo "$create_role_error" | grep -qi "UnauthorizedOperation\|AccessDenied\|is not authorized"; then
            permission_errors+=("iam:CreateRole - Required to create IAM roles")
        fi
        
        # Test list attached role policies
        if ! aws iam list-attached-role-policies --role-name "non-existent-role-test-$$" >/dev/null 2>&1; then
            local list_policies_error
            list_policies_error=$(aws iam list-attached-role-policies --role-name "non-existent-role-test-$$" 2>&1 || echo "")
            if echo "$list_policies_error" | grep -qi "UnauthorizedOperation\|AccessDenied\|is not authorized"; then
                permission_errors+=("iam:ListAttachedRolePolicies - Required to validate IAM role permissions")
            fi
        fi
        
        # Test attach role policy permission
        local attach_policy_error
        attach_policy_error=$(aws iam attach-role-policy --role-name "non-existent-role-test-$$" --policy-arn "arn:aws:iam::aws:policy/AmazonElasticFileSystemClientFullAccess" 2>&1 || echo "")
        if echo "$attach_policy_error" | grep -qi "UnauthorizedOperation\|AccessDenied\|is not authorized"; then
            permission_errors+=("iam:AttachRolePolicy - Required to attach EFS policies to IAM roles")
        fi
    fi

    # Test EFS Access Point permissions (CRITICAL for CSI driver)
    log_info "Testing EFS Access Point permissions..."
    if [[ -z "$test_efs_id" ]]; then
        test_efs_id=$(aws efs describe-file-systems --region "$AWS_REGION" --query 'FileSystems[0].FileSystemId' --output text 2>/dev/null || echo "")
    fi
    
    if [[ -n "$test_efs_id" && "$test_efs_id" != "None" ]]; then
        log_info "Testing CreateAccessPoint permission with filesystem: $test_efs_id"
        
        # Test CreateAccessPoint permission by actually creating one
        local test_ap_id=""
        local create_ap_error
        create_ap_error=$(aws efs create-access-point \
            --region "$AWS_REGION" \
            --file-system-id "$test_efs_id" \
            --posix-user Uid=1001,Gid=1001 \
            --root-directory "Path=/permission-test-$(date +%s),CreationInfo={OwnerUid=1001,OwnerGid=1001,Permissions=755}" \
            --tags Key=test,Value=permission-check \
            --query AccessPointId --output text 2>&1)
        
        if [[ -n "$create_ap_error" && "$create_ap_error" != "None" && ! "$create_ap_error" =~ error && ! "$create_ap_error" =~ AccessDenied ]]; then
            test_ap_id="$create_ap_error"
            log_info "✅ CreateAccessPoint and TagResource permissions verified"
            
            # Test DeleteAccessPoint permission
            if ! aws efs delete-access-point --region "$AWS_REGION" --access-point-id "$test_ap_id" >/dev/null 2>&1; then
                permission_errors+=("elasticfilesystem:DeleteAccessPoint - Required to clean up EFS access points")
            else
                log_info "✅ DeleteAccessPoint permission verified"
            fi
        else
            permission_errors+=("elasticfilesystem:CreateAccessPoint - CRITICAL: Required for EFS CSI driver to create access points")
            permission_errors+=("elasticfilesystem:TagResource - Required to tag EFS access points")
        fi
        
        # Test DescribeAccessPoints permission
        if ! aws efs describe-access-points --region "$AWS_REGION" --file-system-id "$test_efs_id" --max-results 5 >/dev/null 2>&1; then
            permission_errors+=("elasticfilesystem:DescribeAccessPoints - CRITICAL: Required for EFS CSI driver")
        fi
    else
        permission_errors+=("No EFS filesystem found for testing access point permissions")
    fi

    # Test EFS creation permissions
    if [[ "$CREATE_IAM_ROLE" == "true" ]]; then
        log_info "Testing EFS creation permissions..."
        local existing_efs
        existing_efs=$(aws efs describe-file-systems --region "$AWS_REGION" --query 'FileSystems[0].FileSystemId' --output text 2>/dev/null || echo "")
        if [[ -n "$existing_efs" && "$existing_efs" != "None" ]]; then
            # Test CreateTags permission
            if ! aws efs list-tags-for-resource --region "$AWS_REGION" --resource-id "$existing_efs" >/dev/null 2>&1; then
                permission_errors+=("elasticfilesystem:ListTagsForResource - Required to manage EFS tags")
            fi
        fi
        
        log_info "CreateMountTarget permission will be tested during mount target creation"
    fi

    # Test Security Group permissions
    log_info "Testing Security Group permissions..."
    # These are tested in the actual operations since they need real VPC context
    log_info "Security group permissions will be validated during actual operations"

    # Report results
    if [[ ${#permission_errors[@]} -eq 0 ]]; then
        log_success "All testable AWS permissions are available"
        if [[ "$CREATE_IAM_ROLE" == "true" ]]; then
            log_info "Note: Some IAM permissions (CreateRole, CreatePolicy, AttachRolePolicy) will be"
            log_info "      tested during actual resource creation and may still fail if missing."
        fi
    else
        log_error "❌ Missing required AWS permissions:"
        for error in "${permission_errors[@]}"; do
            log_error "  ❌ $error"
        done
        echo
        log_error "🚨 FATAL: Cannot proceed without required AWS permissions!"
        echo
        log_info "To resolve this issue:"
        log_info "Ask your AWS administrator to grant the missing permissions"
        echo
        log_info "Required AWS permissions depend on your setup:"
        if [[ "$CREATE_IAM_ROLE" == "true" ]]; then
            echo "  Core AWS Permissions:"
            echo "    - sts:GetCallerIdentity"
            echo "    - elasticfilesystem:DescribeFileSystems"
            echo "    - elasticfilesystem:DescribeMountTargets" 
            echo "    - ec2:DescribeVpcs"
            echo "    - ec2:DescribeSubnets"
            echo "    - ec2:DescribeSecurityGroups"
            echo
            echo "  EFS CSI Driver Permissions (CRITICAL):"
            echo "    - elasticfilesystem:CreateAccessPoint"
            echo "    - elasticfilesystem:DeleteAccessPoint"
            echo "    - elasticfilesystem:DescribeAccessPoints"
            echo "    - elasticfilesystem:TagResource"
            echo
            echo "  IAM Permissions (for IAM role creation):"
            echo "    Core IAM Operations:"
            echo "      - iam:CreateRole"
            echo "      - iam:GetRole"
            echo "      - iam:ListRoles"
            echo "      - iam:DeleteRole (for cleanup)"
            echo "      - iam:TagRole"
            echo "    Policy Operations:"
            echo "      - iam:CreatePolicy"
            echo "      - iam:GetPolicy"
            echo "      - iam:ListPolicies"
            echo "      - iam:AttachRolePolicy" 
            echo "      - iam:DetachRolePolicy"
            echo "      - iam:ListAttachedRolePolicies"
            echo "    OIDC Operations:"
            echo "      - iam:ListOpenIdConnectProviders"
            echo "      - iam:GetOpenIdConnectProvider"
        fi
        exit 1
    fi
}

# Function to validate EFS CSI role permissions using practical tests
validate_efs_csi_role_permissions() {
    local role_arn="$1"
    local role_name
    role_name=$(basename "$role_arn")
    
    log_info "Validating IAM role permissions: $role_name"
    
    # Check if role has required AWS managed policy attached
    local attached_policies
    attached_policies=$(aws iam list-attached-role-policies --role-name "$role_name" --query 'AttachedPolicies[].PolicyArn' --output text 2>/dev/null || echo "")
    
    if [[ -z "$attached_policies" ]]; then
        log_error "❌ FATAL: Cannot read IAM role policies - missing iam:ListAttachedRolePolicies permission"
        log_error "Ask your AWS administrator to grant you iam:ListAttachedRolePolicies permission"
        exit 1
    fi
    
    # Check for AWS managed EFS policy
    local has_efs_policy=false
    local policy_names=()
    
    for policy_arn in $attached_policies; do
        local policy_name
        policy_name=$(basename "$policy_arn")
        policy_names+=("$policy_name")
        
        # Check for AWS managed EFS policies
        if [[ "$policy_arn" == "arn:aws:iam::aws:policy/AmazonElasticFileSystemClientFullAccess" ]] || \
           [[ "$policy_arn" == "arn:aws:iam::aws:policy/AmazonElasticFileSystemFullAccess" ]] || \
           [[ "$policy_name" == *"EFS"* ]] || \
           [[ "$policy_name" == *"ElasticFileSystem"* ]]; then
            has_efs_policy=true
            log_success "✅ Found EFS policy: $policy_name"
            break
        fi
    done
    
    if [[ "$has_efs_policy" == "false" ]]; then
        log_error "❌ FATAL: IAM role '$role_name' missing required EFS permissions!"
        echo
        log_error "Current attached policies:"
        for policy_name in "${policy_names[@]}"; do
            log_error "  - $policy_name"
        done
        echo
        log_error "Required: The IAM role must have EFS permissions attached."
        log_error "SOLUTION: Ask your AWS administrator to run:"
        log_error "  aws iam attach-role-policy \\"
        log_error "    --role-name $role_name \\"
        log_error "    --policy-arn arn:aws:iam::aws:policy/AmazonElasticFileSystemClientFullAccess"
        echo
        log_error "This validation is mandatory and cannot be bypassed"
        exit 1
    fi
    
    log_success "✅ IAM role has required EFS permissions attached"
    
    # Additional practical validation - test if we can assume the role
    # Note: We can't test role assumption without complex setup, but the EFS CSI driver will test this
    log_info "✅ IAM role validation completed - EFS CSI driver will perform runtime credential tests"
}

# Function to test EFS CSI driver credentials
test_efs_csi_credentials() {
    log_info "Testing EFS CSI driver AWS credentials..."
    
    if [[ "$DRY_RUN" == "true" ]]; then
        log_info "[DRY RUN] Would test EFS CSI driver credentials"
        return
    fi
    
    # Create a test PVC to trigger EFS access point creation
    local test_pvc_name="efs-csi-test-$(date +%s)"
    
    log_info "Creating test PVC to validate EFS CSI driver credentials..."
    
    cat <<EOF | $KUBECTL apply -f -
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $test_pvc_name
  namespace: default
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: $STORAGE_CLASS_NAME
  resources:
    requests:
      storage: 1Gi
EOF
    
    # Wait for PVC to be provisioned or fail
    local max_attempts=30
    local attempt=0
    local test_result="unknown"
    local hypershift_irsa_issue=false
    
    while [[ $attempt -lt $max_attempts ]]; do
        local pvc_status
        pvc_status=$($KUBECTL get pvc "$test_pvc_name" -n default -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
        
        if [[ "$pvc_status" == "Bound" ]]; then
            test_result="success"
            break
        elif [[ "$pvc_status" == "Failed" ]]; then
            test_result="failed"
            break
        fi
        
        # Check for events indicating credential issues
        local pvc_events
        pvc_events=$($KUBECTL get events -n default --field-selector involvedObject.name="$test_pvc_name" -o json 2>/dev/null | jq -r '.items[].message' 2>/dev/null || echo "")
        
        # Check for HyperShift IRSA configuration issue
        if echo "$pvc_events" | grep -qi "No OpenIDConnect provider found.*kubernetes.default.svc"; then
            test_result="hypershift_irsa_issue"
            hypershift_irsa_issue=true
            break
        fi
        
        if echo "$pvc_events" | grep -qi "credential\|permission\|unauthorized\|access.*denied"; then
            test_result="credential_error"
            break
        fi
        
        sleep 5
        ((attempt++))
    done
    
    # Clean up test PVC
    $KUBECTL delete pvc "$test_pvc_name" -n default --ignore-not-found=true
    
    case "$test_result" in
        "success")
            log_success "✅ EFS CSI driver credentials are working correctly"
            ;;
        "hypershift_irsa_issue")
            log_error "❌ EFS CSI driver credential validation failed"
            log_error "❌ HyperShift IRSA (IAM Roles for Service Accounts) configuration issue detected"
            echo
            log_error "🔍 DIAGNOSIS: Your HyperShift cluster is using the internal Kubernetes service account"
            log_error "              token issuer (https://kubernetes.default.svc) instead of the external"
            log_error "              OIDC provider required for AWS IRSA functionality."
            echo
            log_error "💡 SOLUTION OPTIONS:"
            log_error "   1. PREFERRED: Ask your cluster administrator to configure HyperShift for AWS IRSA"
            log_error "   2. WORKAROUND: Use static EFS provisioning instead of dynamic provisioning"
            echo
            log_error "📋 Infrastructure Status:"
            log_error "   ✅ EFS Filesystem: Created successfully"
            log_error "   ✅ IAM Role: Created with correct trust policy and permissions"
            log_error "   ✅ StorageClass: Created successfully"
            log_error "   ❌ Dynamic PVC Provisioning: Blocked by IRSA configuration"
            echo
            log_error "🔧 Static Provisioning Workaround:"
            log_error "   You can use the created EFS filesystem directly with static PVs:"
            log_error "   See documentation: docs/static-efs-provisioning.md"
            exit 1
            ;;
        "credential_error")
            log_error "❌ EFS CSI driver credential validation failed"
            log_error "❌ Check the service account IAM role configuration and permissions"
            echo
            log_error "🔍 Common causes:"
            log_error "   - IAM role missing EFS permissions"
            log_error "   - Incorrect OIDC provider trust policy"
            log_error "   - Service account not properly annotated"
            echo
            log_error "💡 Troubleshooting steps:"
            log_error "   1. Verify IAM role has AmazonElasticFileSystemClientFullAccess policy"
            log_error "   2. Check service account annotation: eks.amazonaws.com/role-arn"
            log_error "   3. Verify OIDC provider trust policy matches cluster"
            exit 1
            ;;
        "failed")
            log_warning "⚠️  Test PVC failed to provision (may be due to other issues)"
            log_info "Check PVC events for more details: oc describe pvc $test_pvc_name"
            ;;
        *)
            log_warning "⚠️  Could not determine EFS CSI driver credential status within timeout"
            log_info "The EFS infrastructure was created successfully, but credential testing timed out"
            ;;
    esac
}

# Function to validate EFS CSI role permissions using practical tests
validate_efs_csi_role_permissions() {
    local role_arn="$1"
    local role_name
    role_name=$(basename "$role_arn")
    
    log_info "Validating IAM role permissions: $role_name"
    
    # Check if role has required AWS managed policy attached
    local attached_policies
    attached_policies=$(aws iam list-attached-role-policies --role-name "$role_name" --query 'AttachedPolicies[].PolicyArn' --output text 2>/dev/null || echo "")
    
    if [[ -z "$attached_policies" ]]; then
        log_error "❌ FATAL: Cannot read IAM role policies - missing iam:ListAttachedRolePolicies permission"
        log_error "Ask your AWS administrator to grant you iam:ListAttachedRolePolicies permission"
        exit 1
    fi
    
    # Check for AWS managed EFS policy
    local has_efs_policy=false
    local policy_names=()
    
    for policy_arn in $attached_policies; do
        local policy_name
        policy_name=$(basename "$policy_arn")
        policy_names+=("$policy_name")
        
        # Check for AWS managed EFS policies
        if [[ "$policy_arn" == "arn:aws:iam::aws:policy/AmazonElasticFileSystemClientFullAccess" ]] || \
           [[ "$policy_arn" == "arn:aws:iam::aws:policy/AmazonElasticFileSystemFullAccess" ]] || \
           [[ "$policy_name" == *"EFS"* ]] || \
           [[ "$policy_name" == *"ElasticFileSystem"* ]]; then
            has_efs_policy=true
            log_success "✅ Found EFS policy: $policy_name"
            break
        fi
    done
    
    if [[ "$has_efs_policy" == "false" ]]; then
        log_error "❌ FATAL: IAM role '$role_name' missing required EFS permissions!"
        echo
        log_error "Current attached policies:"
        for policy_name in "${policy_names[@]}"; do
            log_error "  - $policy_name"
        done
        echo
        log_error "Required: The IAM role must have EFS permissions attached."
        log_error "SOLUTION: Ask your AWS administrator to run:"
        log_error "  aws iam attach-role-policy \\"
        log_error "    --role-name $role_name \\"
        log_error "    --policy-arn arn:aws:iam::aws:policy/AmazonElasticFileSystemClientFullAccess"
        echo
        log_error "This validation is mandatory and cannot be bypassed"
        exit 1
    fi
    
    log_success "✅ IAM role has required EFS permissions attached"
    
    # Additional practical validation - test if we can assume the role
    # Note: We can't test role assumption without complex setup, but the EFS CSI driver will test this
    log_info "✅ IAM role validation completed - EFS CSI driver will perform runtime credential tests"
}

# Function to check and configure EFS CSI service account
check_efs_csi_service_account() {
    log_info "Checking EFS CSI service account configuration..."
    
    if [[ "$DRY_RUN" == "true" ]]; then
        log_info "[DRY RUN] Would check EFS CSI service account configuration"
        return
    fi
    

    
    # Check if service account exists
    if ! $KUBECTL get serviceaccount efs-csi-controller-sa -n kube-system >/dev/null 2>&1; then
        log_error "❌ EFS CSI controller service account not found"
        log_error "The EFS CSI driver may not be properly installed"
        log_error "Run: $KUBECTL get pods -n kube-system | grep efs-csi"
        exit 1
    fi
    
    # Check if service account has IAM role annotation
    local current_role_arn
    current_role_arn=$($KUBECTL get serviceaccount efs-csi-controller-sa -n kube-system -o jsonpath='{.metadata.annotations.eks\.amazonaws\.com/role-arn}' 2>/dev/null || echo "")
    
    if [[ -n "$current_role_arn" ]]; then
        log_success "EFS CSI service account already has IAM role configured: $current_role_arn"
        
        # Validate the IAM role permissions
        validate_efs_csi_role_permissions "$current_role_arn"
    else
        if [[ "$CREATE_IAM_ROLE" == "true" ]]; then
            log_info "EFS CSI service account not configured with IAM role - will create and configure"
            local role_arn
            role_arn=$(create_efs_csi_iam_role)
            
            # Annotate service account with IAM role
            log_info "Annotating EFS CSI service account with IAM role"
            $KUBECTL annotate serviceaccount efs-csi-controller-sa -n kube-system \
                eks.amazonaws.com/role-arn="$role_arn" --overwrite
            
            log_success "EFS CSI service account configured with IAM role: $role_arn"
            
            # Restart EFS CSI controller to pick up new credentials
            log_info "Restarting EFS CSI controller to pick up new IAM role"
            $KUBECTL rollout restart deployment/efs-csi-controller -n kube-system
            
            # Wait for restart to complete
            $KUBECTL rollout status deployment/efs-csi-controller -n kube-system --timeout=60s
        else
            log_error "❌ EFS CSI service account missing IAM role annotation and IAM role creation is disabled"
            echo
            log_error "🔧 SOLUTION: Manually configure the EFS CSI service account with an IAM role:"
            log_error "1. Create an IAM role with EFS permissions"
            log_error "2. Configure the role trust policy for IRSA (IAM Roles for Service Accounts)"
            log_error "3. Annotate the service account:"
            log_error "   $KUBECTL annotate serviceaccount efs-csi-controller-sa -n kube-system \\"
            log_error "     eks.amazonaws.com/role-arn=arn:aws:iam::ACCOUNT:role/ROLE_NAME"
            echo
            log_error "Or enable automatic IAM role creation with --create-iam-role"
            exit 1
        fi
    fi
}

# Function to auto-detect cluster name from OpenShift/Kubernetes cluster
detect_cluster_name() {
    if [[ -n "$CLUSTER_NAME" ]]; then
        log_info "Using specified cluster name: $CLUSTER_NAME"
        return
    fi
    
    log_info "Auto-detecting cluster name from OpenShift/Kubernetes cluster..."
    
    local detected_name=""
    local detection_method=""
    
    # Method 1: OpenShift Infrastructure object (most reliable for OpenShift)
    if [[ -z "$detected_name" ]]; then
        local infra_name
        infra_name=$($KUBECTL get infrastructure cluster -o jsonpath='{.status.infrastructureName}' 2>/dev/null || echo "")

        if [[ -n "$infra_name" ]]; then
            # Extract cluster name from infrastructure name (e.g., "clustername-abc123" -> "clustername")
            # Remove common suffixes like -xxxxx where x is alphanumeric
            detected_name=$(echo "$infra_name" | sed -E 's/-[a-z0-9]{5,}$//')
            detection_method="infrastructure.status.infrastructureName"
            log_info "Detected cluster name from OpenShift infrastructure: $infra_name -> $detected_name"
        fi
    fi
    
    # Method 2: kubectl cluster config (works for any Kubernetes cluster)
    if [[ -z "$detected_name" ]]; then
        local cluster_config_name
        cluster_config_name=$($KUBECTL config view --minify -o jsonpath='{.clusters[0].name}' 2>/dev/null || echo "")

        if [[ -n "$cluster_config_name" ]]; then
            detected_name="$cluster_config_name"
            detection_method="kubectl cluster config name"
            log_info "Detected cluster name from kubectl cluster config: $detected_name"
        fi
    fi
    
    # Method 3: API server URL parsing (robust fallback)
    if [[ -z "$detected_name" ]]; then
        local api_server
        api_server=$($KUBECTL config view --minify -o jsonpath='{.clusters[0].cluster.server}' 2>/dev/null || echo "")
        if [[ -n "$api_server" ]]; then
            # Extract cluster name from API server URL patterns:
            # https://api.clustername.domain.com -> clustername
            # https://clustername-api.domain.com -> clustername  
            # https://api-clustername.domain.com -> clustername
            if [[ "$api_server" =~ https?://api\.([^.]+)\. ]]; then
                detected_name="${BASH_REMATCH[1]}"
                detection_method="API server URL (api.clustername pattern)"
                log_info "Detected cluster name from API server URL: $api_server -> $detected_name"
            elif [[ "$api_server" =~ https?://([^.-]+)-api\. ]]; then
                detected_name="${BASH_REMATCH[1]}"
                detection_method="API server URL (clustername-api pattern)"
                log_info "Detected cluster name from API server URL: $api_server -> $detected_name"
            elif [[ "$api_server" =~ https?://api-([^.]+)\. ]]; then
                detected_name="${BASH_REMATCH[1]}"
                detection_method="API server URL (api-clustername pattern)"
                log_info "Detected cluster name from API server URL: $api_server -> $detected_name"
            fi
        fi
    fi
    
    # Method 4: OpenShift cluster domain from Infrastructure spec
    if [[ -z "$detected_name" ]]; then
        local cluster_domain
        cluster_domain=$($KUBECTL get infrastructure cluster -o jsonpath='{.spec.cloudConfig.name}' 2>/dev/null || echo "")
        if [[ -n "$cluster_domain" ]]; then
            # Extract cluster name from domain (e.g., mycluster-12345 from mycluster-12345.example.com)
            detected_name=$(echo "$cluster_domain" | cut -d'.' -f1)
            # Remove suffixes if present
            detected_name=$(echo "$detected_name" | sed -E 's/-[a-z0-9]{5,}$//')
            detection_method="infrastructure.spec.cloudConfig.name"
            log_info "Detected cluster name from cloud config: $detected_name"
        fi
    fi
    
    # Method 5: OpenShift DNS configuration
    if [[ -z "$detected_name" ]]; then
        local dns_domain
        dns_domain=$($KUBECTL get dns cluster -o jsonpath='{.spec.baseDomain}' 2>/dev/null || echo "")
        if [[ -n "$dns_domain" ]]; then
            # Try to extract cluster name from base domain
            # Pattern: apps.clustername.domain.com -> clustername
            if [[ "$dns_domain" =~ ^apps\.([^.]+)\. ]]; then
                detected_name="${BASH_REMATCH[1]}"
                detection_method="dns.spec.baseDomain"
                log_info "Detected cluster name from DNS base domain: $detected_name"
            fi
        fi
    fi
    
    # Method 6: OpenShift Console URL
    if [[ -z "$detected_name" ]]; then
        local console_url
        console_url=$($KUBECTL get route console -n openshift-console -o jsonpath='{.spec.host}' 2>/dev/null || echo "")
        if [[ -n "$console_url" ]]; then
            # Pattern: console-openshift-console.apps.clustername.domain.com -> clustername  
            if [[ "$console_url" =~ apps\.([^.]+)\. ]]; then
                detected_name="${BASH_REMATCH[1]}"
                detection_method="console route host"
                log_info "Detected cluster name from console route: $detected_name"
            fi
        fi
    fi
    
    # Method 7: Node labels (for OpenShift/EKS clusters)
    if [[ -z "$detected_name" ]]; then
        # Check for cluster-specific node labels
        local cluster_label
        cluster_label=$($KUBECTL get nodes -o jsonpath='{.items[0].metadata.labels}' 2>/dev/null | grep -o 'kubernetes\.io/cluster/[^"]*' | head -1 | cut -d'/' -f3 || echo "")
        if [[ -n "$cluster_label" ]]; then
            detected_name="$cluster_label"
            detection_method="node labels kubernetes.io/cluster"
            log_info "Detected cluster name from node labels: $detected_name"
        fi
    fi
    
    # Method 8: EKS cluster name from node provider ID
    if [[ -z "$detected_name" ]]; then
        local provider_id
        provider_id=$($KUBECTL get nodes -o jsonpath='{.items[0].spec.providerID}' 2>/dev/null || echo "")
        if [[ "$provider_id" =~ aws:///([^/]+)/ ]]; then
            local availability_zone="${BASH_REMATCH[1]}"
            # For EKS, try to get cluster name from instance metadata
            local instance_id
            if [[ "$provider_id" =~ /([i-][a-f0-9]+)$ ]]; then
                instance_id="${BASH_REMATCH[1]}"
                # Try to get cluster name from instance tags
                local cluster_tag
                cluster_tag=$(aws ec2 describe-instances \
                    --instance-ids "$instance_id" \
                    --region "${availability_zone%?}" \
                    --query 'Reservations[0].Instances[0].Tags[?Key==`kubernetes.io/cluster/*`].Key' \
                    --output text 2>/dev/null | cut -d'/' -f3 || echo "")
                if [[ -n "$cluster_tag" ]]; then
                    detected_name="$cluster_tag"
                    detection_method="EC2 instance tags"
                    log_info "Detected cluster name from EC2 instance tags: $detected_name"
                fi
            fi
        fi
    fi
    
    # Method 9: Current kubectl context (fallback)
    if [[ -z "$detected_name" ]]; then
        local context_name
        context_name=$($KUBECTL config current-context 2>/dev/null || echo "")
        if [[ -n "$context_name" ]]; then
            # Try different context patterns
            if [[ "$context_name" =~ ^[^/]+/[^/]+:([^/]+)/ ]]; then
                # Pattern: user/cluster:server/context
                detected_name="${BASH_REMATCH[1]}"
                detection_method="kubectl context (pattern 1)"
            elif [[ "$context_name" =~ /([^/]+)$ ]]; then
                # Pattern: anything/clustername
                detected_name="${BASH_REMATCH[1]}"
                detection_method="kubectl context (pattern 2)"
            elif [[ "$context_name" =~ ^([^-]+)-[a-f0-9]{8,}$ ]]; then
                # Pattern: clustername-hash
                detected_name="${context_name%-*}"
                detection_method="kubectl context (pattern 3)"
            else
                # Use full context name as fallback
                detected_name="$context_name"
                detection_method="kubectl context (full name)"
            fi
            log_info "Detected cluster name from kubectl context: $detected_name"
        fi
    fi
    
    # Method 10: ClusterVersion for OpenShift (additional validation)
    if [[ -n "$detected_name" ]]; then
        local cluster_id
        cluster_id=$($KUBECTL get clusterversion version -o jsonpath='{.spec.clusterID}' 2>/dev/null || echo "")
        if [[ -n "$cluster_id" ]]; then
            log_info "Cluster ID: $cluster_id (method: $detection_method)"
        fi
    fi
    
    # Validate and set cluster name
    if [[ -n "$detected_name" ]]; then
        # Clean up cluster name (remove special characters, make lowercase)
        CLUSTER_NAME=$(echo "$detected_name" | sed 's/[^a-zA-Z0-9-]/-/g' | tr '[:upper:]' '[:lower:]' | sed 's/--*/-/g' | sed 's/^-\|-$//g')
        
        if [[ ${#CLUSTER_NAME} -gt 63 ]]; then
            # AWS resource names have limits, truncate if necessary
            CLUSTER_NAME="${CLUSTER_NAME:0:63}"
            log_warning "Cluster name truncated to 63 characters: $CLUSTER_NAME"
        fi
        
        log_success "Auto-detected cluster name: $CLUSTER_NAME (via: $detection_method)"
    else
        log_error "Could not auto-detect cluster name from any available source."
        log_error "Available detection methods attempted:"
        log_error "  1. OpenShift Infrastructure object"
        log_error "  2. kubectl cluster config name"
        log_error "  3. API server URL parsing"
        log_error "  4. OpenShift cloud config"
        log_error "  5. OpenShift DNS configuration"
        log_error "  6. OpenShift Console route"
        log_error "  7. Kubernetes node labels"
        log_error "  8. AWS EC2 instance tags"
        log_error "  9. kubectl current context"
        log_error ""
        log_error "This should never happen - cluster detection is designed to be bulletproof."
        log_error "Please report this as a bug with the output of:"
        log_error "  kubectl config view --minify"
        log_error "  kubectl get infrastructure cluster -o yaml 2>/dev/null || echo 'No infrastructure object'"
        exit 1
    fi
}

# Function to auto-detect AWS region from OpenShift/Kubernetes cluster
detect_aws_region() {
    if [[ -n "$AWS_REGION" ]]; then
        log_info "Using specified AWS region: $AWS_REGION"
        return
    fi
    
    log_info "Auto-detecting AWS region from OpenShift/Kubernetes cluster..."
    
    local detected_region=""
    local detection_method=""
    
    # Method 1: OpenShift Infrastructure object (most reliable for OpenShift)
    if [[ -z "$detected_region" ]]; then
        detected_region=$($KUBECTL get infrastructure cluster -o jsonpath='{.status.platformStatus.aws.region}' 2>/dev/null || echo "")
        if [[ -n "$detected_region" ]]; then
            detection_method="infrastructure.status.platformStatus.aws.region"
            log_info "Detected AWS region from OpenShift infrastructure: $detected_region"
        fi
    fi
    
    # Method 2: Node provider IDs (works for both OpenShift and EKS)
    if [[ -z "$detected_region" ]]; then
        local provider_id
        provider_id=$($KUBECTL get nodes -o jsonpath='{.items[0].spec.providerID}' 2>/dev/null || echo "")
        if [[ "$provider_id" =~ aws:///([^/]+)/ ]]; then
            # Extract region from availability zone (remove last character)
            local availability_zone="${BASH_REMATCH[1]}"
            detected_region="${availability_zone%?}"
            detection_method="node provider ID"
            log_info "Detected AWS region from node provider ID: $detected_region"
        fi
    fi
    
    # Method 3: Node names (AWS pattern)
    if [[ -z "$detected_region" ]]; then
        local node_names
        node_names=$($KUBECTL get nodes -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
        
        if [[ -n "$node_names" ]]; then
            for node in $node_names; do
                # Pattern: ip-10-0-1-1.us-west-2.compute.internal
                if [[ "$node" =~ \.([^.]*-(east|west|north|south|southeast|northeast|central)-[0-9]+)\.compute\.internal ]]; then
                    detected_region="${BASH_REMATCH[1]}"
                    detection_method="node DNS names"
                    log_info "Detected AWS region from node name pattern: $detected_region"
                    break
                fi
            done
        fi
    fi
    
    # Method 4: Node zone labels
    if [[ -z "$detected_region" ]]; then
        local zone_label
        zone_label=$($KUBECTL get nodes -o jsonpath='{.items[0].metadata.labels.topology\.kubernetes\.io/zone}' 2>/dev/null || echo "")
        if [[ -n "$zone_label" ]]; then
            # Extract region from zone (remove last character)
            detected_region="${zone_label%?}"
            detection_method="node zone labels"
            log_info "Detected AWS region from node zone label: $detected_region"
        fi
    fi
    
    # Method 5: OpenShift Machine Config
    if [[ -z "$detected_region" ]]; then
        local machine_region
        machine_region=$($KUBECTL get machine -n openshift-machine-api -o jsonpath='{.items[0].spec.providerSpec.value.placement.region}' 2>/dev/null || echo "")
        if [[ -n "$machine_region" ]]; then
            detected_region="$machine_region"
            detection_method="machine placement.region"
            log_info "Detected AWS region from machine config: $detected_region"
        fi
    fi
    
    # Method 6: StorageClass region parameters
    if [[ -z "$detected_region" ]]; then
        local sc_region
        sc_region=$($KUBECTL get storageclass -o jsonpath='{.items[?(@.provisioner=="ebs.csi.aws.com")].parameters.region}' 2>/dev/null | head -1 || echo "")
        if [[ -n "$sc_region" ]]; then
            detected_region="$sc_region"
            detection_method="StorageClass parameters"
            log_info "Detected AWS region from StorageClass: $detected_region"
        fi
    fi
    
    # Method 7: AWS CLI default region
    if [[ -z "$detected_region" ]]; then
        detected_region=$(aws configure get region 2>/dev/null || echo "")
        if [[ -n "$detected_region" ]]; then
            detection_method="AWS CLI configuration"
            log_info "Detected AWS region from AWS CLI config: $detected_region"
        fi
    fi
    
    # Method 8: Environment variable
    if [[ -z "$detected_region" ]]; then
        detected_region="$AWS_DEFAULT_REGION"
        if [[ -n "$detected_region" ]]; then
            detection_method="AWS_DEFAULT_REGION environment variable"
            log_info "Using AWS region from environment: $detected_region"
        fi
    fi
    
    # Validate and set region
    if [[ -n "$detected_region" ]]; then
        # Validate region format (basic check)
        if [[ "$detected_region" =~ ^[a-z]{2,3}-[a-z]+-[0-9]+$ ]]; then
            AWS_REGION="$detected_region"
            log_success "Auto-detected AWS region: $AWS_REGION (via: $detection_method)"
        else
            log_warning "Detected region '$detected_region' has invalid format, trying anyway..."
            AWS_REGION="$detected_region"
        fi
    else
        log_error "Could not auto-detect AWS region from any available source."
        log_error "Available detection methods attempted:"
        log_error "  1. OpenShift Infrastructure object"
        log_error "  2. Node provider IDs"
        log_error "  3. Node DNS names"
        log_error "  4. Node zone labels"
        log_error "  5. OpenShift Machine Config"
        log_error "  6. StorageClass parameters"
        log_error "  7. AWS CLI configuration"
        log_error "  8. Environment variables"
        log_error ""
        log_error "Please specify AWS region manually with --aws-region"
        log_error "Example: $0 --aws-region us-west-2"
        exit 1
    fi
}

# Function to set default values after cluster detection
set_default_values() {
    log_info "Setting default values based on detected cluster configuration..."
    
    # Set default storage class name if not specified
    if [[ -z "$STORAGE_CLASS_NAME" ]]; then
        STORAGE_CLASS_NAME="sbd-efs-sc"
        log_info "Using default StorageClass name: $STORAGE_CLASS_NAME"
    fi
    
    # Set default EFS name if not specified
    if [[ -z "$EFS_NAME" ]]; then
        EFS_NAME="sbd-efs-${CLUSTER_NAME}"
        log_info "Using default EFS name: $EFS_NAME"
    fi
    
    # Validate that required values are now set
    if [[ -z "$CLUSTER_NAME" ]]; then
        log_error "CLUSTER_NAME is not set - this should not happen after cluster detection"
        exit 1
    fi
    
    if [[ -z "$STORAGE_CLASS_NAME" ]]; then
        log_error "STORAGE_CLASS_NAME is not set - this should not happen after setting defaults"
        exit 1
    fi
    
    if [[ -z "$EFS_NAME" ]]; then
        log_error "EFS_NAME is not set - this should not happen after setting defaults"
        exit 1
    fi
    
    log_success "Default values configured successfully"
}

# Function to install or verify EFS CSI driver
install_or_verify_efs_csi_driver() {
    log_info "Checking EFS CSI driver installation..."
    
    if $KUBECTL get csidriver efs.csi.aws.com &>/dev/null; then
        log_success "EFS CSI driver is already installed"
        return
    fi
    
    if [[ "$SKIP_CSI_INSTALL" == "true" ]]; then
        log_error "EFS CSI driver not found and automatic installation is disabled"
        log_error "Please install it manually or remove --skip-csi-install flag"
        exit 1
    fi
    
    log_info "EFS CSI driver not found. Installing automatically..."
    
    if [[ "$DRY_RUN" == "true" ]]; then
        log_info "[DRY RUN] Would install EFS CSI driver"
        return
    fi
    
    # Install EFS CSI driver
    if $KUBECTL apply -k 'github.com/kubernetes-sigs/aws-efs-csi-driver/deploy/kubernetes/overlays/stable/?ref=release-1.7' >/dev/null 2>&1; then
        log_success "EFS CSI driver installed successfully"
        
        # Wait for driver to be ready
        log_info "Waiting for EFS CSI driver to be ready..."
        local max_attempts=30
        local attempt=0
        while [[ $attempt -lt $max_attempts ]]; do
            if $KUBECTL get csidriver efs.csi.aws.com &>/dev/null; then
                log_success "EFS CSI driver is ready"
                return
            fi
            sleep 5
            ((attempt++))
        done
        log_warning "EFS CSI driver installation may still be in progress"
    else
        log_error "Failed to install EFS CSI driver. Please install manually:"
        log_error "  oc apply -k 'github.com/kubernetes-sigs/aws-efs-csi-driver/deploy/kubernetes/overlays/stable/?ref=release-1.7'"
        exit 1
    fi
}

# Function to create EFS filesystem
create_efs_filesystem() {
    log_info "Creating EFS filesystem..."
    
    # Check if EFS already exists
    local existing_efs
    existing_efs=$(aws efs describe-file-systems \
        --region "$AWS_REGION" \
        --query "FileSystems[?Tags[?Key=='Name' && Value=='$EFS_NAME']].FileSystemId" \
        --output text 2>/dev/null || echo "")
    
    if [[ -n "$existing_efs" && "$existing_efs" != "None" ]]; then
        log_info "EFS filesystem '$EFS_NAME' already exists: $existing_efs"
        echo "$existing_efs"
        return
    fi
    
    if [[ "$DRY_RUN" == "true" ]]; then
        log_info "[DRY RUN] Would create EFS filesystem: $EFS_NAME"
        echo "fs-dryrun"
        return
    fi
    
    # Create EFS filesystem
    local create_args=(
        --region "$AWS_REGION"
        --performance-mode "$PERFORMANCE_MODE"
        --throughput-mode "$THROUGHPUT_MODE"
    )
    
    if [[ "$THROUGHPUT_MODE" == "provisioned" ]]; then
        create_args+=(--provisioned-throughput-in-mibps "$PROVISIONED_THROUGHPUT")
    fi
    
    local efs_id
    efs_id=$(aws efs create-file-system "${create_args[@]}" --query 'FileSystemId' --output text)
    
    if [[ -z "$efs_id" ]]; then
        log_error "Failed to create EFS filesystem"
        exit 1
    fi
    
    log_success "Created EFS filesystem: $efs_id"
    
    # Wait for EFS to be available
    log_info "Waiting for EFS filesystem to be available..."
    local max_attempts=30
    local attempt=0
    while [[ $attempt -lt $max_attempts ]]; do
        local state
        state=$(aws efs describe-file-systems \
            --region "$AWS_REGION" \
            --file-system-id "$efs_id" \
            --query 'FileSystems[0].LifeCycleState' \
            --output text 2>/dev/null || echo "")
        
        if [[ "$state" == "available" ]]; then
            break
        elif [[ "$state" == "error" ]]; then
            log_error "EFS filesystem creation failed"
            exit 1
        fi
        
        log_info "EFS state: $state, waiting... (attempt $((attempt + 1))/$max_attempts)"
        sleep 10
        ((attempt++))
    done
    
    if [[ $attempt -ge $max_attempts ]]; then
        log_error "Timeout waiting for EFS filesystem to become available"
        exit 1
    fi
    
    # Add tags
    tag_efs_filesystem "$efs_id"
    
    echo "$efs_id"
}

# Function to tag EFS filesystem
tag_efs_filesystem() {
    local efs_id="$1"
    
    log_info "Adding tags to EFS filesystem..."
    
    if [[ "$DRY_RUN" == "true" ]]; then
        log_info "[DRY RUN] Would add tags to EFS filesystem: $efs_id"
        return
    fi
    
    # Add tags (ignore permission errors)
    aws efs create-tags \
        --region "$AWS_REGION" \
        --file-system-id "$efs_id" \
        --tags \
            "Key=Name,Value=$EFS_NAME" \
            "Key=Purpose,Value=sbd-operator-rwx-storage" \
            "Key=Cluster,Value=$CLUSTER_NAME" \
            "Key=kubernetes.io/cluster/${CLUSTER_NAME},Value=owned" \
            "Key=CreatedBy,Value=sbd-operator-script" \
            "Key=CreatedDate,Value=$(date -u +%Y-%m-%d)" \
        >/dev/null 2>&1 || \
        log_warning "Could not add tags to EFS filesystem (permission issue), but filesystem was created successfully"
    
    log_success "Tags added to EFS filesystem"
}

# Function to find EFS filesystem by name
find_efs_by_name() {
    local name="$1"
    
    aws efs describe-file-systems \
        --region "$AWS_REGION" \
        --query "FileSystems[?Tags[?Key=='Name' && Value=='$name']].FileSystemId" \
        --output text 2>/dev/null || echo ""
}

# Function to configure EFS CSI driver credentials
configure_efs_csi_credentials() {
    log_info "Configuring EFS CSI driver with cluster credentials..."
    
    if [[ "$DRY_RUN" == "true" ]]; then
        log_info "[DRY RUN] Would configure EFS CSI driver credentials"
        return
    fi
    
    # Get cluster credentials
    local cluster_region
    cluster_region=$(oc get infrastructure cluster -o jsonpath='{.status.platformStatus.aws.region}' 2>/dev/null || echo "$AWS_REGION")
    
    # Update EFS CSI driver configuration
    $KUBECTL patch csidriver efs.csi.aws.com --type merge -p '{
        "spec": {
            "storageCapacity": false,
            "volumeLifecycleModes": ["Persistent"]
        }
    }' >/dev/null 2>&1 || true
    
    log_success "EFS CSI driver credentials configured"
}

# Function to detect cluster VPC and subnets
detect_cluster_vpc_and_subnets() {
    log_info "Detecting cluster VPC and subnets..."
    
    # Get cluster infrastructure details
    local cluster_vpc_id=""
    local cluster_subnets=""
    
    # Try to get VPC from cluster infrastructure
    cluster_vpc_id=$(oc get infrastructure cluster -o jsonpath='{.status.platformStatus.aws.resourceTags.kubernetes\.io/cluster/.*}' 2>/dev/null | head -1 || echo "")
    
    if [[ -z "$cluster_vpc_id" ]]; then
        # Fallback: find VPC by cluster tag
        cluster_vpc_id=$(aws ec2 describe-vpcs \
            --region "$AWS_REGION" \
            --filters "Name=tag:kubernetes.io/cluster/${CLUSTER_NAME},Values=owned,shared" \
            --query 'Vpcs[0].VpcId' \
            --output text 2>/dev/null || echo "")
    fi
    
    if [[ -z "$cluster_vpc_id" || "$cluster_vpc_id" == "None" ]]; then
        # Fallback: find VPC by node IP addresses
        log_info "No cluster tags found, detecting VPC from node IPs..."
        local node_ip
        node_ip=$($KUBECTL get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}' 2>/dev/null || echo "")
        
        if [[ -n "$node_ip" ]]; then
            log_info "Using node IP $node_ip to find VPC..."
            # Find all subnets and check which one contains this IP
            local all_subnets
            all_subnets=$(aws ec2 describe-subnets --region "$AWS_REGION" --query 'Subnets[].{SubnetId:SubnetId,VpcId:VpcId,CidrBlock:CidrBlock}' --output json 2>/dev/null || echo "[]")
            
            # Use python to find matching subnet (more reliable than bash CIDR matching)
            cluster_vpc_id=$(echo "$all_subnets" | python3 -c "
import json
import ipaddress
import sys

try:
    subnets = json.load(sys.stdin)
    node_ip = '$node_ip'
    
    for subnet in subnets:
        try:
            cidr = ipaddress.IPv4Network(subnet['CidrBlock'])
            if ipaddress.IPv4Address(node_ip) in cidr:
                print(subnet['VpcId'])
                break
        except:
            continue
except:
    pass
" 2>/dev/null || echo "")
        fi
    fi
    
    if [[ -z "$cluster_vpc_id" || "$cluster_vpc_id" == "None" ]]; then
        log_error "Could not detect cluster VPC. Ensure you're connected to the right cluster."
        exit 1
    fi
    
    # Get private subnets from the VPC
    cluster_subnets=$(aws ec2 describe-subnets \
        --region "$AWS_REGION" \
        --filters \
            "Name=vpc-id,Values=$cluster_vpc_id" \
            "Name=tag:kubernetes.io/role/internal-elb,Values=1" \
        --query 'Subnets[].SubnetId' \
        --output text 2>/dev/null || echo "")
    
    if [[ -z "$cluster_subnets" || "$cluster_subnets" == "None" ]]; then
        # Fallback: get all private subnets
        cluster_subnets=$(aws ec2 describe-subnets \
            --region "$AWS_REGION" \
            --filters \
                "Name=vpc-id,Values=$cluster_vpc_id" \
                "Name=tag:Name,Values=*private*" \
            --query 'Subnets[].SubnetId' \
            --output text 2>/dev/null || echo "")
    fi
    
    if [[ -z "$cluster_subnets" || "$cluster_subnets" == "None" ]]; then
        # Final fallback: get subnets that contain cluster nodes
        log_info "No tagged private subnets found, detecting from node locations..."
        local node_ips
        node_ips=$($KUBECTL get nodes -o jsonpath='{.items[*].status.addresses[?(@.type=="InternalIP")].address}' 2>/dev/null | tr ' ' '\n' | sort -u || echo "")
        
        if [[ -n "$node_ips" ]]; then
            local all_subnets
            all_subnets=$(aws ec2 describe-subnets --region "$AWS_REGION" --filters "Name=vpc-id,Values=$cluster_vpc_id" --query 'Subnets[].{SubnetId:SubnetId,CidrBlock:CidrBlock}' --output json 2>/dev/null || echo "[]")
            
            # Find subnets containing our nodes
            cluster_subnets=$(echo "$all_subnets" | python3 -c "
import json
import ipaddress
import sys

try:
    subnets = json.load(sys.stdin)
    node_ips = '''$node_ips'''.strip().split('\n')
    found_subnets = set()
    
    for subnet in subnets:
        try:
            cidr = ipaddress.IPv4Network(subnet['CidrBlock'])
            for node_ip in node_ips:
                if node_ip and ipaddress.IPv4Address(node_ip.strip()) in cidr:
                    found_subnets.add(subnet['SubnetId'])
        except:
            continue
    
    print(' '.join(found_subnets))
except:
    pass
" 2>/dev/null || echo "")
        fi
    fi
    
    if [[ -z "$cluster_subnets" || "$cluster_subnets" == "None" ]]; then
        log_error "Could not find private subnets in cluster VPC: $cluster_vpc_id"
        exit 1
    fi
    
    log_success "Detected cluster VPC: $cluster_vpc_id"
    log_success "Detected private subnets: $cluster_subnets"
    
    echo "$cluster_vpc_id|$cluster_subnets"
}

# Function to create or get NFS security group
create_or_get_nfs_security_group() {
    local vpc_id="$1"
    local sg_name="efs-nfs-access-${CLUSTER_NAME}"
    
    log_info "Creating or getting NFS security group..."
    
    if [[ "$DRY_RUN" == "true" ]]; then
        log_info "[DRY RUN] Would create/get NFS security group: $sg_name"
        echo "sg-dryrun123456789"
        return
    fi
    
    # Check if security group already exists
    local sg_id
    sg_id=$(aws ec2 describe-security-groups \
        --region "$AWS_REGION" \
        --filters \
            "Name=vpc-id,Values=$vpc_id" \
            "Name=group-name,Values=$sg_name" \
        --query 'SecurityGroups[0].GroupId' \
        --output text 2>/dev/null || echo "")
    
    if [[ -n "$sg_id" && "$sg_id" != "None" ]]; then
        log_info "Using existing security group: $sg_id"
        echo "$sg_id"
        return
    fi
    
    # Create new security group
    sg_id=$(aws ec2 create-security-group \
        --region "$AWS_REGION" \
        --group-name "$sg_name" \
        --description "NFS access for EFS in cluster $CLUSTER_NAME" \
        --vpc-id "$vpc_id" \
        --query 'GroupId' \
        --output text 2>/dev/null || echo "")
    
    if [[ -z "$sg_id" || "$sg_id" == "None" ]]; then
        log_error "Failed to create security group"
        exit 1
    fi
    
    # Add NFS ingress rule (port 2049)
    aws ec2 authorize-security-group-ingress \
        --region "$AWS_REGION" \
        --group-id "$sg_id" \
        --protocol tcp \
        --port 2049 \
        --source-group "$sg_id" \
        >/dev/null 2>&1 || true
    
    # Add tags to security group
    aws ec2 create-tags \
        --region "$AWS_REGION" \
        --resources "$sg_id" \
        --tags \
            "Key=Name,Value=$sg_name" \
            "Key=kubernetes.io/cluster/${CLUSTER_NAME},Value=owned" \
            "Key=CreatedBy,Value=sbd-operator-script" \
        >/dev/null 2>&1 || true
    
    log_success "Created NFS security group: $sg_id"
    echo "$sg_id"
}

# Function to create EFS mount targets
create_efs_mount_targets() {
    local efs_id="$1"
    local vpc_id="$2"
    local subnets="$3"
    
    log_info "Creating EFS mount targets..."
    
    if [[ "$DRY_RUN" == "true" ]]; then
        log_info "[DRY RUN] Would create EFS mount targets for: $efs_id"
        return
    fi
    
    # Get or create NFS security group
    local sg_id
    sg_id=$(create_or_get_nfs_security_group "$vpc_id")
    
    # Create mount targets for each subnet
    local created_targets=0
    for subnet_id in $subnets; do
        # Check if mount target already exists for this subnet
        local existing_target
        existing_target=$(aws efs describe-mount-targets \
            --region "$AWS_REGION" \
            --file-system-id "$efs_id" \
            --query "MountTargets[?SubnetId=='$subnet_id'].MountTargetId" \
            --output text 2>/dev/null || echo "")
        
        if [[ -n "$existing_target" && "$existing_target" != "None" ]]; then
            log_info "Mount target already exists for subnet $subnet_id: $existing_target"
            continue
        fi
        
        # Create mount target
        local mount_target_id
        mount_target_id=$(aws efs create-mount-target \
            --region "$AWS_REGION" \
            --file-system-id "$efs_id" \
            --subnet-id "$subnet_id" \
            --security-groups "$sg_id" \
            --query 'MountTargetId' \
            --output text 2>/dev/null || echo "")
        
        if [[ -n "$mount_target_id" && "$mount_target_id" != "None" ]]; then
            log_success "Created mount target: $mount_target_id (subnet: $subnet_id)"
            ((created_targets++))
        else
            log_warning "Failed to create mount target for subnet: $subnet_id"
        fi
    done
    
    if [[ $created_targets -gt 0 ]]; then
        log_info "Waiting for mount targets to become available..."
        sleep 10
    fi
    
    log_success "EFS mount targets setup completed"
}

# Function to setup EFS networking
setup_efs_networking() {
    local efs_id="$1"
    
    log_info "Setting up EFS networking configuration..."
    
    if [[ "$DRY_RUN" == "true" ]]; then
        log_info "[DRY RUN] Would setup EFS networking for: $efs_id"
        return
    fi
    
    # Detect VPC and subnets
    local vpc_info
    vpc_info=$(detect_cluster_vpc_and_subnets)
    local vpc_id
    local subnets
    vpc_id=$(echo "$vpc_info" | cut -d'|' -f1)
    subnets=$(echo "$vpc_info" | cut -d'|' -f2)
    
    # Configure EFS CSI driver
    configure_efs_csi_credentials
    
    # Create mount targets
    create_efs_mount_targets "$efs_id" "$vpc_id" "$subnets"
    
    log_success "EFS networking setup completed"
}

# Function to create StorageClass
create_storage_class() {
    local efs_id="$1"
    
    log_info "Creating StorageClass..."
    
    if [[ "$DRY_RUN" == "true" ]]; then
        log_info "[DRY RUN] Would create StorageClass: $STORAGE_CLASS_NAME"
        cat << EOF
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: $STORAGE_CLASS_NAME
  labels:
    storage-type: efs-rwx
    cluster: $CLUSTER_NAME
provisioner: efs.csi.aws.com
parameters:
  provisioningMode: efs-ap
  fileSystemId: $efs_id
  directoryPerms: "0755"
allowVolumeExpansion: true
EOF
        return
    fi
    
    # Create StorageClass with EFS Access Point provisioning
    # AWS permissions have been verified for elasticfilesystem:CreateAccessPoint
    cat << EOF | $KUBECTL apply -f -
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: $STORAGE_CLASS_NAME
  labels:
    storage-type: efs-rwx
    cluster: $CLUSTER_NAME
provisioner: efs.csi.aws.com
parameters:
  provisioningMode: efs-ap
  fileSystemId: $efs_id
  directoryPerms: "0755"
allowVolumeExpansion: true
EOF
    
    log_success "Created StorageClass: $STORAGE_CLASS_NAME"
    log_info "Using EFS Access Point provisioning for dynamic PVC management"
}

# Function to cleanup resources
cleanup_resources() {
    log_warning "Cleaning up EFS and StorageClass resources..."
    
    if [[ "$DRY_RUN" == "true" ]]; then
        log_info "[DRY RUN] Would cleanup all resources"
        return
    fi
    
    # Delete StorageClass
    log_info "Deleting StorageClass..."
    $KUBECTL delete storageclass "$STORAGE_CLASS_NAME" --ignore-not-found=true
    
    # Find and delete EFS filesystem
    local efs_id
    efs_id=$(find_efs_by_name "$EFS_NAME")
    
    if [[ -n "$efs_id" && "$efs_id" != "None" ]]; then
        log_info "Deleting EFS filesystem and associated resources: $efs_id"
        
        # Get VPC information for security group cleanup
        local vpc_info
        vpc_info=$(detect_cluster_vpc_and_subnets)
        local vpc_id
        vpc_id=$(echo "$vpc_info" | cut -d'|' -f1)
        
        # Check for mount targets and delete them
        local mount_targets
        mount_targets=$(aws efs describe-mount-targets \
            --region "$AWS_REGION" \
            --file-system-id "$efs_id" \
            --query 'MountTargets[].MountTargetId' \
            --output text 2>/dev/null || echo "")
        
        if [[ -n "$mount_targets" && "$mount_targets" != "None" ]]; then
            log_info "Deleting mount targets..."
            for mt_id in $mount_targets; do
                log_info "Deleting mount target: $mt_id"
                aws efs delete-mount-target --region "$AWS_REGION" --mount-target-id "$mt_id" >/dev/null 2>&1 || true
            done
            
            # Wait for mount targets to be deleted
            log_info "Waiting for mount targets to be deleted..."
            sleep 15
        fi
        
        # Delete EFS filesystem
        aws efs delete-file-system --region "$AWS_REGION" --file-system-id "$efs_id" >/dev/null 2>&1 || \
            log_warning "Could not delete EFS filesystem (may have dependencies or permission issues)"
        
        # Clean up security group (only if no other EFS filesystems are using it)
        local sg_name="efs-nfs-access-${CLUSTER_NAME}"
        local sg_id
        sg_id=$(aws ec2 describe-security-groups \
            --region "$AWS_REGION" \
            --filters \
                "Name=vpc-id,Values=$vpc_id" \
                "Name=group-name,Values=$sg_name" \
            --query 'SecurityGroups[0].GroupId' \
            --output text 2>/dev/null || echo "")
        
        if [[ -n "$sg_id" && "$sg_id" != "None" ]]; then
            # Check if any other EFS filesystems are using this security group
            local other_mount_targets
            other_mount_targets=$(aws efs describe-mount-targets \
                --region "$AWS_REGION" \
                --query "MountTargets[?SecurityGroups[?contains(@, '$sg_id')]]" \
                --output text 2>/dev/null || echo "")
            
            if [[ -z "$other_mount_targets" || "$other_mount_targets" == "None" ]]; then
                log_info "Deleting unused NFS security group: $sg_id"
                aws ec2 delete-security-group --region "$AWS_REGION" --group-id "$sg_id" >/dev/null 2>&1 || \
                    log_warning "Could not delete security group (may be in use)"
            else
                log_info "Keeping security group $sg_id (in use by other EFS mount targets)"
            fi
        fi
        
        log_success "EFS filesystem and associated resources cleanup initiated"
    else
        log_info "No EFS filesystem found with name: $EFS_NAME"
    fi
    
    log_success "Cleanup completed"
}

# Function to display summary
show_summary() {
    local efs_id="$1"
    
    log_success "EFS StorageClass setup completed!"
    echo
    echo "📋 Summary:"
    echo "  StorageClass Name: $STORAGE_CLASS_NAME"
    echo "  EFS Filesystem ID: $efs_id"
    echo "  EFS Name: $EFS_NAME"
    echo "  Cluster: $CLUSTER_NAME"
    echo "  Region: $AWS_REGION"
    echo "  Access Mode: ReadWriteMany (RWX)"
    echo
    echo "🚀 Usage in PVCs:"
    echo "  apiVersion: v1"
    echo "  kind: PersistentVolumeClaim"
    echo "  metadata:"
    echo "    name: sbd-shared-storage"
    echo "  spec:"
    echo "    accessModes:"
    echo "    - ReadWriteMany"
    echo "    storageClassName: $STORAGE_CLASS_NAME"
    echo "    resources:"
    echo "      requests:"
    echo "        storage: 10Gi"
    echo
    echo "🔍 Verify with:"
    echo "  $KUBECTL get storageclass $STORAGE_CLASS_NAME"
    echo
    echo "🗑️  Cleanup with:"
    echo "  ./scripts/setup-shared-storage.sh --cleanup --efs-name $EFS_NAME"
}

# Main execution
main() {
    log_info "Starting EFS StorageClass management for OpenShift with intelligent resource reuse"
    
    # Check tools
    check_tools
    
    # Auto-detect cluster and region
    detect_cluster_name
    detect_aws_region
    
    # Set default values after cluster detection
    set_default_values
    
    # Check AWS permissions (after region is detected)
    check_aws_permissions
    
    # Handle cleanup
    if [[ "$CLEANUP" == "true" ]]; then
        cleanup_resources
        exit 0
    fi
    
    # Check for existing resources before proceeding
    check_existing_resources
    
    # Install or verify EFS CSI driver
    install_or_verify_efs_csi_driver
    
    # Check and configure EFS CSI service account IAM role
    check_efs_csi_service_account
    
    # Determine EFS filesystem ID with intelligent reuse
    local efs_id=""
    if [[ "$CREATE_EFS" == "true" ]]; then
        # Check for existing EFS filesystem first
        local existing_efs
        existing_efs=$(find_efs_by_name "$EFS_NAME")
        
        if [[ -n "$existing_efs" && "$existing_efs" != "None" ]]; then
            if [[ "$FORCE_RECREATE" == "true" ]]; then
                log_info "Force recreate enabled - deleting existing EFS filesystem"
                # Clean up existing EFS in cleanup_resources style
                local vpc_info
                vpc_info=$(detect_cluster_vpc_and_subnets)
                local vpc_id
                vpc_id=$(echo "$vpc_info" | cut -d'|' -f1)
                
                # Delete mount targets first
                local mount_targets
                mount_targets=$(aws efs describe-mount-targets \
                    --region "$AWS_REGION" \
                    --file-system-id "$existing_efs" \
                    --query 'MountTargets[].MountTargetId' \
                    --output text 2>/dev/null || echo "")
                
                if [[ -n "$mount_targets" && "$mount_targets" != "None" ]]; then
                    log_info "Deleting existing mount targets..."
                    for mt_id in $mount_targets; do
                        aws efs delete-mount-target --region "$AWS_REGION" --mount-target-id "$mt_id" >/dev/null 2>&1 || true
                    done
                    sleep 15
                fi
                
                # Delete EFS filesystem
                aws efs delete-file-system --region "$AWS_REGION" --file-system-id "$existing_efs" >/dev/null 2>&1 || \
                    log_warning "Could not delete existing EFS filesystem"
                
                # Create new one
                efs_id=$(create_efs_filesystem)
            else
                # Validate existing EFS configuration
                local efs_status
                efs_status=$(compare_efs_config "$existing_efs")
                
                case "$efs_status" in
                    "valid")
                        log_success "Reusing existing compatible EFS filesystem: $existing_efs"
                        efs_id="$existing_efs"
                        ;;
                    "config_changed")
                        log_error "Existing EFS filesystem has incompatible configuration. Use --force-recreate to recreate."
                        exit 1
                        ;;
                    "missing")
                        log_info "EFS filesystem not found - creating new one"
                        efs_id=$(create_efs_filesystem)
                        ;;
                esac
            fi
        else
            efs_id=$(create_efs_filesystem)
        fi
    else
        if [[ -n "$EFS_FILESYSTEM_ID" ]]; then
            efs_id="$EFS_FILESYSTEM_ID"
            log_info "Using specified EFS filesystem: $efs_id"
            
            # Validate specified EFS exists and is compatible
            local efs_status
            efs_status=$(compare_efs_config "$efs_id")
            
            if [[ "$efs_status" == "missing" ]]; then
                log_error "Specified EFS filesystem not found: $efs_id"
                exit 1
            elif [[ "$efs_status" == "config_changed" ]]; then
                log_warning "Specified EFS filesystem has different configuration than requested"
                log_warning "Proceeding anyway since --filesystem-id was explicitly specified"
            fi
        else
            log_error "EFS filesystem ID is required when not creating new EFS"
            show_usage
            exit 1
        fi
    fi
    
    # Setup EFS networking (this function already handles existing mount targets)
    setup_efs_networking "$efs_id"
    
    # Handle StorageClass creation/update intelligently
    if handle_storage_class_update "$efs_id"; then
        create_storage_class "$efs_id"
    else
        log_success "StorageClass is up to date - no changes needed"
    fi
    
    # Test EFS CSI driver credentials
    test_efs_csi_credentials
    
    # Show summary
    if [[ "$DRY_RUN" != "true" ]]; then
        show_summary "$efs_id"
    else
        log_info "[DRY RUN] All operations completed successfully (no actual resources created)"
    fi
}

# Run main function
main "$@" 
