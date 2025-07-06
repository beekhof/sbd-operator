package aws

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/efs"
	efstypes "github.com/aws/aws-sdk-go-v2/service/efs/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
)

// Config holds AWS-specific configuration
type Config struct {
	Region                string
	ClusterName           string
	EFSName               string
	PerformanceMode       string
	ThroughputMode        string
	ProvisionedThroughput int64
	EFSCSIRoleName        string
}

// NetworkResult contains networking setup results
type NetworkResult struct {
	MountTargets    []string
	SecurityGroupID string
}

// Manager handles AWS operations
type Manager struct {
	config    *Config
	awsConfig aws.Config
	efsClient *efs.Client
	iamClient *iam.Client
	ec2Client *ec2.Client
}

// NewManager creates a new AWS manager
func NewManager(ctx context.Context, cfg *Config) (*Manager, error) {
	// Load AWS configuration
	awsConfig, err := config.LoadDefaultConfig(ctx, config.WithRegion(cfg.Region))
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	return &Manager{
		config:    cfg,
		awsConfig: awsConfig,
		efsClient: efs.NewFromConfig(awsConfig),
		iamClient: iam.NewFromConfig(awsConfig),
		ec2Client: ec2.NewFromConfig(awsConfig),
	}, nil
}

// ValidatePermissions checks if the required AWS permissions are available
func (m *Manager) ValidatePermissions(ctx context.Context) error {
	// Test basic AWS permissions
	if _, err := m.efsClient.DescribeFileSystems(ctx, &efs.DescribeFileSystemsInput{
		MaxItems: aws.Int32(1),
	}); err != nil {
		return fmt.Errorf("EFS permissions check failed: %w", err)
	}

	if _, err := m.iamClient.ListRoles(ctx, &iam.ListRolesInput{
		MaxItems: aws.Int32(1),
	}); err != nil {
		return fmt.Errorf("IAM permissions check failed: %w", err)
	}

	if _, err := m.ec2Client.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{
		MaxResults: aws.Int32(1),
	}); err != nil {
		return fmt.Errorf("EC2 permissions check failed: %w", err)
	}

	return nil
}

// CreateEFS creates a new EFS filesystem
func (m *Manager) CreateEFS(ctx context.Context) (string, error) {
	// Check if EFS already exists
	existingEFS, err := m.findEFSByName(ctx, m.config.EFSName)
	if err != nil {
		return "", err
	}
	if existingEFS != "" {
		log.Printf("📁 Found existing EFS filesystem: %s", existingEFS)
		return existingEFS, nil
	}

	// Create EFS filesystem
	throughputMode := efstypes.ThroughputMode(m.config.ThroughputMode)
	performanceMode := efstypes.PerformanceMode(m.config.PerformanceMode)

	input := &efs.CreateFileSystemInput{
		CreationToken:   aws.String(fmt.Sprintf("%s-%d", m.config.EFSName, time.Now().Unix())),
		PerformanceMode: performanceMode,
		ThroughputMode:  throughputMode,
		Tags: []efstypes.Tag{
			{
				Key:   aws.String("Name"),
				Value: aws.String(m.config.EFSName),
			},
			{
				Key:   aws.String("Cluster"),
				Value: aws.String(m.config.ClusterName),
			},
			{
				Key:   aws.String("Purpose"),
				Value: aws.String("SBD-SharedStorage"),
			},
		},
	}

	if throughputMode == efstypes.ThroughputModeProvisioned {
		input.ProvisionedThroughputInMibps = aws.Float64(float64(m.config.ProvisionedThroughput))
	}

	result, err := m.efsClient.CreateFileSystem(ctx, input)
	if err != nil {
		return "", fmt.Errorf("failed to create EFS filesystem: %w", err)
	}

	efsID := *result.FileSystemId
	log.Printf("📁 Created EFS filesystem: %s", efsID)

	// Wait for filesystem to become available
	if err := m.waitForEFSAvailable(ctx, efsID); err != nil {
		return "", fmt.Errorf("EFS filesystem did not become available: %w", err)
	}

	return efsID, nil
}

// ValidateEFS validates an existing EFS filesystem
func (m *Manager) ValidateEFS(ctx context.Context, efsID string) error {
	result, err := m.efsClient.DescribeFileSystems(ctx, &efs.DescribeFileSystemsInput{
		FileSystemId: aws.String(efsID),
	})
	if err != nil {
		return fmt.Errorf("failed to describe EFS filesystem: %w", err)
	}

	if len(result.FileSystems) == 0 {
		return fmt.Errorf("EFS filesystem %s not found", efsID)
	}

	fs := result.FileSystems[0]
	if fs.LifeCycleState != efstypes.LifeCycleStateAvailable {
		return fmt.Errorf("EFS filesystem %s is not available (state: %s)", efsID, fs.LifeCycleState)
	}

	return nil
}

// SetupNetworking configures VPC, subnets, security groups, and mount targets for EFS
func (m *Manager) SetupNetworking(ctx context.Context, efsID string) (*NetworkResult, error) {
	// Get cluster VPC and subnets
	vpcInfo, err := m.detectClusterVPC(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to detect cluster VPC: %w", err)
	}

	// Create or find security group for EFS
	securityGroupID, err := m.ensureEFSSecurityGroup(ctx, vpcInfo.VPCID)
	if err != nil {
		return nil, fmt.Errorf("failed to ensure EFS security group: %w", err)
	}

	// Create mount targets
	mountTargets, err := m.createMountTargets(ctx, efsID, vpcInfo.SubnetIDs, securityGroupID)
	if err != nil {
		return nil, fmt.Errorf("failed to create mount targets: %w", err)
	}

	return &NetworkResult{
		MountTargets:    mountTargets,
		SecurityGroupID: securityGroupID,
	}, nil
}

// SetupIAMRole creates and configures IAM role for EFS CSI driver
func (m *Manager) SetupIAMRole(ctx context.Context) (string, error) {
	// Check if role already exists
	roleARN, err := m.findIAMRole(ctx, m.config.EFSCSIRoleName)
	if err != nil {
		return "", err
	}
	if roleARN != "" {
		log.Printf("🔐 Found existing IAM role: %s", roleARN)
		return roleARN, nil
	}

	// Get OIDC provider for the cluster
	oidcProvider, err := m.detectOIDCProvider(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to detect OIDC provider: %w", err)
	}

	// Create trust policy for IRSA
	trustPolicy := fmt.Sprintf(`{
		"Version": "2012-10-17",
		"Statement": [
			{
				"Effect": "Allow",
				"Principal": {
					"Federated": "%s"
				},
				"Action": "sts:AssumeRoleWithWebIdentity",
				"Condition": {
					"StringEquals": {
						"%s:sub": "system:serviceaccount:kube-system:efs-csi-controller-sa",
						"%s:aud": "sts.amazonaws.com"
					}
				}
			}
		]
	}`, oidcProvider, strings.TrimPrefix(oidcProvider, "arn:aws:iam::"), strings.TrimPrefix(oidcProvider, "arn:aws:iam::"))

	// Create IAM role
	createRoleInput := &iam.CreateRoleInput{
		RoleName:                 aws.String(m.config.EFSCSIRoleName),
		AssumeRolePolicyDocument: aws.String(trustPolicy),
		Description:              aws.String("IAM role for EFS CSI driver"),
		Tags: []iamtypes.Tag{
			{
				Key:   aws.String("Cluster"),
				Value: aws.String(m.config.ClusterName),
			},
			{
				Key:   aws.String("Purpose"),
				Value: aws.String("EFS-CSI-Driver"),
			},
		},
	}

	createRoleResult, err := m.iamClient.CreateRole(ctx, createRoleInput)
	if err != nil {
		return "", fmt.Errorf("failed to create IAM role: %w", err)
	}

	roleARN = *createRoleResult.Role.Arn
	log.Printf("🔐 Created IAM role: %s", roleARN)

	// Attach EFS policy to the role
	policyARN := "arn:aws:iam::aws:policy/AmazonElasticFileSystemClientFullAccess"
	_, err = m.iamClient.AttachRolePolicy(ctx, &iam.AttachRolePolicyInput{
		RoleName:  aws.String(m.config.EFSCSIRoleName),
		PolicyArn: aws.String(policyARN),
	})
	if err != nil {
		return "", fmt.Errorf("failed to attach EFS policy to role: %w", err)
	}

	log.Printf("🔐 Attached EFS policy to role: %s", policyARN)

	return roleARN, nil
}

// Cleanup removes all AWS resources created by this manager
func (m *Manager) Cleanup(ctx context.Context) error {
	// Find and delete EFS filesystem
	efsID, err := m.findEFSByName(ctx, m.config.EFSName)
	if err != nil {
		log.Printf("Warning: failed to find EFS for cleanup: %v", err)
	} else if efsID != "" {
		if err := m.deleteEFS(ctx, efsID); err != nil {
			log.Printf("Warning: failed to delete EFS: %v", err)
		}
	}

	// Delete IAM role
	if err := m.deleteIAMRole(ctx, m.config.EFSCSIRoleName); err != nil {
		log.Printf("Warning: failed to delete IAM role: %v", err)
	}

	return nil
}

// Helper methods

type VPCInfo struct {
	VPCID     string
	SubnetIDs []string
}

func (m *Manager) detectClusterVPC(ctx context.Context) (*VPCInfo, error) {
	// Find VPC tagged with cluster name
	result, err := m.ec2Client.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{
		Filters: []types.Filter{
			{
				Name:   aws.String(fmt.Sprintf("tag:kubernetes.io/cluster/%s", m.config.ClusterName)),
				Values: []string{"shared", "owned"},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to describe VPCs: %w", err)
	}

	if len(result.Vpcs) == 0 {
		return nil, fmt.Errorf("no VPC found for cluster %s", m.config.ClusterName)
	}

	vpcID := *result.Vpcs[0].VpcId

	// Find subnets in this VPC
	subnetsResult, err := m.ec2Client.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		Filters: []types.Filter{
			{
				Name:   aws.String("vpc-id"),
				Values: []string{vpcID},
			},
			{
				Name:   aws.String(fmt.Sprintf("tag:kubernetes.io/cluster/%s", m.config.ClusterName)),
				Values: []string{"shared", "owned"},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to describe subnets: %w", err)
	}

	var subnetIDs []string
	for _, subnet := range subnetsResult.Subnets {
		subnetIDs = append(subnetIDs, *subnet.SubnetId)
	}

	if len(subnetIDs) == 0 {
		return nil, fmt.Errorf("no subnets found for cluster %s", m.config.ClusterName)
	}

	return &VPCInfo{
		VPCID:     vpcID,
		SubnetIDs: subnetIDs,
	}, nil
}

func (m *Manager) ensureEFSSecurityGroup(ctx context.Context, vpcID string) (string, error) {
	groupName := fmt.Sprintf("%s-efs-sg", m.config.ClusterName)

	// Check if security group already exists
	result, err := m.ec2Client.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		Filters: []types.Filter{
			{
				Name:   aws.String("group-name"),
				Values: []string{groupName},
			},
			{
				Name:   aws.String("vpc-id"),
				Values: []string{vpcID},
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to describe security groups: %w", err)
	}

	if len(result.SecurityGroups) > 0 {
		return *result.SecurityGroups[0].GroupId, nil
	}

	// Create security group
	createResult, err := m.ec2Client.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName:   aws.String(groupName),
		Description: aws.String("Security group for EFS access"),
		VpcId:       aws.String(vpcID),
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeSecurityGroup,
				Tags: []types.Tag{
					{
						Key:   aws.String("Name"),
						Value: aws.String(groupName),
					},
					{
						Key:   aws.String("Cluster"),
						Value: aws.String(m.config.ClusterName),
					},
				},
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to create security group: %w", err)
	}

	securityGroupID := *createResult.GroupId

	// Add inbound rule for NFS (port 2049)
	_, err = m.ec2Client.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(securityGroupID),
		IpPermissions: []types.IpPermission{
			{
				IpProtocol: aws.String("tcp"),
				FromPort:   aws.Int32(2049),
				ToPort:     aws.Int32(2049),
				IpRanges: []types.IpRange{
					{
						CidrIp:      aws.String("10.0.0.0/8"),
						Description: aws.String("NFS access from private networks"),
					},
				},
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to authorize security group ingress: %w", err)
	}

	log.Printf("🛡️ Created security group: %s", securityGroupID)
	return securityGroupID, nil
}

func (m *Manager) createMountTargets(ctx context.Context, efsID string, subnetIDs []string, securityGroupID string) ([]string, error) {
	var mountTargets []string

	for _, subnetID := range subnetIDs {
		// Check if mount target already exists
		existing, err := m.efsClient.DescribeMountTargets(ctx, &efs.DescribeMountTargetsInput{
			FileSystemId: aws.String(efsID),
		})
		if err != nil {
			return nil, fmt.Errorf("failed to describe mount targets: %w", err)
		}

		// Check if mount target already exists in this subnet
		var exists bool
		for _, mt := range existing.MountTargets {
			if *mt.SubnetId == subnetID {
				mountTargets = append(mountTargets, *mt.MountTargetId)
				exists = true
				break
			}
		}

		if exists {
			continue
		}

		// Create mount target
		result, err := m.efsClient.CreateMountTarget(ctx, &efs.CreateMountTargetInput{
			FileSystemId:   aws.String(efsID),
			SubnetId:       aws.String(subnetID),
			SecurityGroups: []string{securityGroupID},
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create mount target in subnet %s: %w", subnetID, err)
		}

		mountTargets = append(mountTargets, *result.MountTargetId)
		log.Printf("🔗 Created mount target: %s in subnet %s", *result.MountTargetId, subnetID)
	}

	return mountTargets, nil
}

func (m *Manager) findEFSByName(ctx context.Context, name string) (string, error) {
	result, err := m.efsClient.DescribeFileSystems(ctx, &efs.DescribeFileSystemsInput{})
	if err != nil {
		return "", fmt.Errorf("failed to describe file systems: %w", err)
	}

	for _, fs := range result.FileSystems {
		// Check tags for name
		tags, err := m.efsClient.DescribeTags(ctx, &efs.DescribeTagsInput{
			FileSystemId: fs.FileSystemId,
		})
		if err != nil {
			continue
		}

		for _, tag := range tags.Tags {
			if *tag.Key == "Name" && *tag.Value == name {
				return *fs.FileSystemId, nil
			}
		}
	}

	return "", nil
}

func (m *Manager) waitForEFSAvailable(ctx context.Context, efsID string) error {
	for i := 0; i < 60; i++ { // Wait up to 5 minutes
		result, err := m.efsClient.DescribeFileSystems(ctx, &efs.DescribeFileSystemsInput{
			FileSystemId: aws.String(efsID),
		})
		if err != nil {
			return err
		}

		if len(result.FileSystems) > 0 && result.FileSystems[0].LifeCycleState == efstypes.LifeCycleStateAvailable {
			return nil
		}

		time.Sleep(5 * time.Second)
	}

	return fmt.Errorf("EFS filesystem did not become available within timeout")
}

func (m *Manager) findIAMRole(ctx context.Context, roleName string) (string, error) {
	result, err := m.iamClient.GetRole(ctx, &iam.GetRoleInput{
		RoleName: aws.String(roleName),
	})
	if err != nil {
		// Role doesn't exist
		return "", nil
	}

	return *result.Role.Arn, nil
}

func (m *Manager) detectOIDCProvider(ctx context.Context) (string, error) {
	// List OIDC providers and find one matching the cluster
	result, err := m.iamClient.ListOpenIDConnectProviders(ctx, &iam.ListOpenIDConnectProvidersInput{})
	if err != nil {
		return "", fmt.Errorf("failed to list OIDC providers: %w", err)
	}

	for _, provider := range result.OpenIDConnectProviderList {
		// Get provider details
		details, err := m.iamClient.GetOpenIDConnectProvider(ctx, &iam.GetOpenIDConnectProviderInput{
			OpenIDConnectProviderArn: provider.Arn,
		})
		if err != nil {
			continue
		}

		// Check if this provider is for our cluster
		if details.Url != nil && strings.Contains(*details.Url, m.config.ClusterName) {
			return *provider.Arn, nil
		}
	}

	return "", fmt.Errorf("no OIDC provider found for cluster %s", m.config.ClusterName)
}

func (m *Manager) deleteEFS(ctx context.Context, efsID string) error {
	// Delete mount targets first
	mountTargets, err := m.efsClient.DescribeMountTargets(ctx, &efs.DescribeMountTargetsInput{
		FileSystemId: aws.String(efsID),
	})
	if err == nil {
		for _, mt := range mountTargets.MountTargets {
			_, err := m.efsClient.DeleteMountTarget(ctx, &efs.DeleteMountTargetInput{
				MountTargetId: mt.MountTargetId,
			})
			if err != nil {
				log.Printf("Warning: failed to delete mount target %s: %v", *mt.MountTargetId, err)
			}
		}
	}

	// Wait for mount targets to be deleted
	time.Sleep(30 * time.Second)

	// Delete filesystem
	_, err = m.efsClient.DeleteFileSystem(ctx, &efs.DeleteFileSystemInput{
		FileSystemId: aws.String(efsID),
	})
	if err != nil {
		return fmt.Errorf("failed to delete EFS filesystem: %w", err)
	}

	log.Printf("🗑️ Deleted EFS filesystem: %s", efsID)
	return nil
}

func (m *Manager) deleteIAMRole(ctx context.Context, roleName string) error {
	// Detach policies first
	policies, err := m.iamClient.ListAttachedRolePolicies(ctx, &iam.ListAttachedRolePoliciesInput{
		RoleName: aws.String(roleName),
	})
	if err == nil {
		for _, policy := range policies.AttachedPolicies {
			_, err := m.iamClient.DetachRolePolicy(ctx, &iam.DetachRolePolicyInput{
				RoleName:  aws.String(roleName),
				PolicyArn: policy.PolicyArn,
			})
			if err != nil {
				log.Printf("Warning: failed to detach policy %s: %v", *policy.PolicyArn, err)
			}
		}
	}

	// Delete role
	_, err = m.iamClient.DeleteRole(ctx, &iam.DeleteRoleInput{
		RoleName: aws.String(roleName),
	})
	if err != nil {
		return fmt.Errorf("failed to delete IAM role: %w", err)
	}

	log.Printf("🗑️ Deleted IAM role: %s", roleName)
	return nil
}
