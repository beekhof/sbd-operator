# AWS EFS Tagging APIs: TagResource vs CreateTags

## Problem Solved

The SBD operator setup tool was failing with:

```
AccessDeniedException: User is not authorized to perform: elasticfilesystem:TagResource
```

This revealed that AWS has **two different EFS tagging APIs** with different permission requirements.

## AWS EFS Tagging API Differences

### 1. **TagResource API (Newer, Unified)**

**Permission:** `elasticfilesystem:TagResource`

**Usage:** Tags resources during creation or at any time using resource ARNs

```go
// Used automatically when creating EFS with tags inline
input := &efs.CreateFileSystemInput{
    CreationToken: aws.String("my-efs"),
    Tags: []efstypes.Tag{    // ← This triggers TagResource permission check
        {Key: aws.String("Name"), Value: aws.String("my-efs")},
    },
}
```

**Also used for explicit tagging:**
```go
// Direct TagResource API call
efs.TagResource(&efs.TagResourceInput{
    ResourceId: aws.String("fs-12345"),
    Tags: []efstypes.Tag{...},
})
```

### 2. **CreateTags API (Legacy)**

**Permission:** `elasticfilesystem:CreateTags`

**Usage:** Tags existing EFS filesystems using filesystem ID

```go
// Legacy API - only works with existing filesystems
efs.CreateTags(&efs.CreateTagsInput{
    FileSystemId: aws.String("fs-12345"),  // Must be existing filesystem
    Tags: []efstypes.Tag{...},
})
```

## When Each Permission is Required

| Operation | Permission Required | API Used |
|-----------|-------------------|----------|
| **Create EFS with tags** | `elasticfilesystem:TagResource` | TagResource (automatic) |
| **Tag existing EFS** | `elasticfilesystem:TagResource` | TagResource |
| **Tag existing EFS (legacy)** | `elasticfilesystem:CreateTags` | CreateTags |

## Why This Matters for SBD Operator

The SBD operator creates EFS filesystems with tags inline for proper resource identification:

```go
// pkg/storage/aws/manager.go - CreateEFS()
input := &efs.CreateFileSystemInput{
    CreationToken: aws.String(fmt.Sprintf("%s-%d", m.config.EFSName, time.Now().Unix())),
    Tags: []efstypes.Tag{
        {Key: aws.String("Name"), Value: aws.String(m.config.EFSName)},           // ← Required for detection
        {Key: aws.String("Cluster"), Value: aws.String(m.config.ClusterName)},    // ← Required for cleanup
        {Key: aws.String("Purpose"), Value: aws.String("SBD-SharedStorage")},     // ← Required for identification
    },
}
```

**Result:** AWS automatically calls TagResource → requires `elasticfilesystem:TagResource` permission

## Required IAM Policy

Both permissions are included in the generated policy for maximum compatibility:

```json
{
  "Sid": "EFSWriteOperations",
  "Effect": "Allow",
  "Action": [
    "elasticfilesystem:CreateFileSystem",
    "elasticfilesystem:CreateMountTarget", 
    "elasticfilesystem:CreateTags",     // Legacy API compatibility
    "elasticfilesystem:TagResource"     // Required for inline tagging during creation
  ],
  "Resource": [
    "arn:aws:elasticfilesystem:*:*:file-system/*",
    "arn:aws:elasticfilesystem:*:*:mount-target/*"
  ]
}
```

## Validation Testing

The tool now tests both permissions:

```go
// Test TagResource (for inline tagging during creation)
func (m *Manager) testTagResource() error {
    _, err := m.efsClient.TagResource(context.Background(), &efs.TagResourceInput{
        ResourceId: aws.String("fs-nonexistent123"), // Invalid ID triggers validation error
        Tags: []efstypes.Tag{{Key: aws.String("Name"), Value: aws.String("test")}},
    })
    return err
}

// Test CreateTags (legacy API)
func (m *Manager) testCreateTags() error {
    _, err := m.efsClient.CreateTags(context.Background(), &efs.CreateTagsInput{
        FileSystemId: aws.String("fs-nonexistent123"), // Invalid ID triggers validation error
        Tags: []efstypes.Tag{{Key: aws.String("Name"), Value: aws.String("test")}},
    })
    return err
}
```

## Migration Notes

**For existing deployments:**
- Update IAM policies to include `elasticfilesystem:TagResource`
- Both `TagResource` and `CreateTags` permissions are recommended for full compatibility
- The tool will fail during EFS creation without `TagResource`

**For new deployments:**
- Use the generated IAM policy which includes both permissions
- `TagResource` is the critical permission for inline tagging during resource creation

## References

- [AWS EFS TagResource API](https://docs.aws.amazon.com/efs/latest/ug/API_TagResource.html)
- [AWS EFS CreateTags API](https://docs.aws.amazon.com/efs/latest/ug/API_CreateTags.html)
- [AWS Resource Tagging Best Practices](https://docs.aws.amazon.com/general/latest/gr/aws_tagging.html) 