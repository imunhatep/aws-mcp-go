package mcpserver

import (
	"strings"
	"time"
	"unicode"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/imunhatep/awslib/service"
	"github.com/imunhatep/awslib/service/autoscaling"
	"github.com/imunhatep/awslib/service/cloudfront"
	"github.com/imunhatep/awslib/service/dynamodb"
	"github.com/imunhatep/awslib/service/ec2"
	"github.com/imunhatep/awslib/service/ecs"
	"github.com/imunhatep/awslib/service/efs"
	"github.com/imunhatep/awslib/service/eks"
	"github.com/imunhatep/awslib/service/elb"
	"github.com/imunhatep/awslib/service/iam"
	"github.com/imunhatep/awslib/service/lambda"
	"github.com/imunhatep/awslib/service/rds"
	"github.com/imunhatep/awslib/service/route53"
	"github.com/imunhatep/awslib/service/secretmanager"
	"github.com/imunhatep/awslib/service/sns"
	"github.com/imunhatep/awslib/service/sqs"
)

// summaryAttributes returns a compact, curated set of the most relevant
// provider-native fields for a resource, keyed by snake_case name. It is a
// best-effort enrichment layered on top of the normalized resourceDTO fields:
// each recognized concrete awslib entity contributes the handful of attributes
// that callers most often need (an EC2 instance's type/state/IPs, an RDS
// engine/class/status, a load balancer's DNS name, …) without the bulk of the
// full raw entity (which the detail flag still provides). Unrecognized types
// return nil, leaving the DTO unchanged.
func summaryAttributes(r service.ResourceInterface) map[string]any {
	m := map[string]any{}

	switch e := r.(type) {
	case ec2.Instance:
		addStr(m, "instance_type", e.GetInstanceType())
		addStr(m, "instance_family", e.GetInstanceFamily())
		addStr(m, "state", e.GetState())
		addStr(m, "private_ip", aws.ToString(e.PrivateIpAddress))
		addStr(m, "public_ip", aws.ToString(e.PublicIpAddress))
		addStr(m, "architecture", string(e.Architecture))
		addStr(m, "vpc_id", aws.ToString(e.VpcId))
		addStr(m, "subnet_id", aws.ToString(e.SubnetId))
		if e.Placement != nil {
			addStr(m, "availability_zone", aws.ToString(e.Placement.AvailabilityZone))
		}

	case ec2.Volume:
		m["size_gb"] = e.GetSize()
		addStr(m, "volume_type", string(e.VolumeType))
		addStr(m, "state", string(e.GetState()))
		addInt32(m, "iops", e.Iops)
		addInt32(m, "throughput_mibps", e.Throughput)
		addBool(m, "encrypted", e.Encrypted)
		addStr(m, "availability_zone", aws.ToString(e.AvailabilityZone))
		addStr(m, "snapshot_id", aws.ToString(e.SnapshotId))

	case ec2.Snapshot:
		addStr(m, "volume_id", aws.ToString(e.VolumeId))
		addInt32(m, "volume_size_gb", e.VolumeSize)
		addStr(m, "state", string(e.State))
		addStr(m, "storage_tier", string(e.StorageTier))
		addBool(m, "encrypted", e.Encrypted)
		addStr(m, "progress", aws.ToString(e.Progress))

	case ec2.Vpc:
		addStr(m, "cidr_block", aws.ToString(e.CidrBlock))
		addStr(m, "state", string(e.State))
		addBool(m, "is_default", e.IsDefault)

	case rds.DbInstance:
		addStr(m, "engine", e.GetEngine())
		addStr(m, "engine_version", e.GetEngineVersion())
		addStr(m, "instance_class", aws.ToString(e.DBInstanceClass))
		addStr(m, "status", aws.ToString(e.DBInstanceStatus))
		addInt32(m, "allocated_storage_gb", e.AllocatedStorage)
		addStr(m, "storage_type", aws.ToString(e.StorageType))
		addBool(m, "multi_az", e.MultiAZ)
		addBool(m, "storage_encrypted", e.StorageEncrypted)
		addBool(m, "publicly_accessible", e.PubliclyAccessible)
		if e.Endpoint != nil {
			addStr(m, "endpoint", aws.ToString(e.Endpoint.Address))
			addInt32(m, "port", e.Endpoint.Port)
		}

	case rds.DbSnapshot:
		addStr(m, "engine", aws.ToString(e.Engine))
		addStr(m, "engine_version", aws.ToString(e.EngineVersion))
		addStr(m, "status", aws.ToString(e.Status))
		addInt32(m, "allocated_storage_gb", e.AllocatedStorage)
		addStr(m, "snapshot_type", aws.ToString(e.SnapshotType))
		addBool(m, "encrypted", e.Encrypted)
		addStr(m, "availability_zone", aws.ToString(e.AvailabilityZone))

	case ecs.Cluster:
		addStr(m, "status", aws.ToString(e.Status))
		m["running_tasks"] = e.RunningTasksCount
		m["pending_tasks"] = e.PendingTasksCount
		m["active_services"] = e.ActiveServicesCount
		m["container_instances"] = e.RegisteredContainerInstancesCount

	case ecs.Service:
		addStr(m, "status", aws.ToString(e.Status))
		m["desired_count"] = e.DesiredCount
		m["running_count"] = e.RunningCount
		m["pending_count"] = e.PendingCount
		addStr(m, "launch_type", string(e.LaunchType))
		addStr(m, "task_definition", aws.ToString(e.TaskDefinition))

	case eks.Cluster:
		if e.Cluster != nil {
			addStr(m, "status", string(e.Status))
			addStr(m, "version", aws.ToString(e.Version))
			addStr(m, "platform_version", aws.ToString(e.PlatformVersion))
			addStr(m, "endpoint", aws.ToString(e.Endpoint))
		}

	case elb.LoadBalancer:
		addStr(m, "lb_type", string(e.LoadBalancer.Type))
		addStr(m, "scheme", string(e.Scheme))
		addStr(m, "dns_name", aws.ToString(e.DNSName))
		addStr(m, "ip_address_type", string(e.IpAddressType))
		addStr(m, "vpc_id", aws.ToString(e.VpcId))
		if e.State != nil {
			addStr(m, "state", string(e.State.Code))
		}

	case route53.HostedZone:
		addInt64(m, "record_count", e.ResourceRecordSetCount)
		if e.Config != nil {
			m["private_zone"] = e.Config.PrivateZone
		}

	case route53.ResourceRecord:
		addStr(m, "record_type", string(e.ResourceRecordSet.Type))
		addInt64(m, "ttl", e.TTL)
		m["record_values"] = len(e.ResourceRecords)
		m["alias"] = e.AliasTarget != nil

	case secretmanager.SecretEntry:
		if e.DescribeSecretOutput != nil {
			addBool(m, "rotation_enabled", e.RotationEnabled)
			addStr(m, "kms_key_id", aws.ToString(e.KmsKeyId))
			addTime(m, "last_changed", e.LastChangedDate)
			addTime(m, "last_rotated", e.LastRotatedDate)
			addTime(m, "next_rotation", e.NextRotationDate)
		}

	case efs.FileSystem:
		addStr(m, "lifecycle_state", string(e.LifeCycleState))
		if e.SizeInBytes != nil {
			m["size_bytes"] = e.SizeInBytes.Value
		}
		m["mount_targets"] = e.NumberOfMountTargets
		addStr(m, "performance_mode", string(e.PerformanceMode))
		addStr(m, "throughput_mode", string(e.ThroughputMode))
		addBool(m, "encrypted", e.Encrypted)

	case lambda.Function:
		addStr(m, "runtime", string(e.Runtime))
		addStr(m, "handler", aws.ToString(e.Handler))
		addInt32(m, "memory_mb", e.MemorySize)
		addInt32(m, "timeout_s", e.Timeout)
		m["code_size_bytes"] = e.CodeSize
		addStr(m, "state", string(e.State))
		addStr(m, "package_type", string(e.PackageType))

	case dynamodb.Table:
		if e.TableDescription != nil {
			addStr(m, "status", string(e.TableStatus))
			addInt64(m, "item_count", e.ItemCount)
			addInt64(m, "size_bytes", e.TableSizeBytes)
			if e.BillingModeSummary != nil {
				addStr(m, "billing_mode", string(e.BillingModeSummary.BillingMode))
			}
			if e.ProvisionedThroughput != nil {
				addInt64(m, "read_capacity", e.ProvisionedThroughput.ReadCapacityUnits)
				addInt64(m, "write_capacity", e.ProvisionedThroughput.WriteCapacityUnits)
			}
		}

	case iam.User:
		addStr(m, "path", aws.ToString(e.Path))
		addStr(m, "user_id", aws.ToString(e.UserId))
		addTime(m, "password_last_used", e.PasswordLastUsed)

	case autoscaling.AutoScalingGroup:
		addStr(m, "asg_name", aws.ToString(e.AutoScalingGroupName))
		addStr(m, "status", aws.ToString(e.Status))
		addInt32(m, "desired_capacity", e.DesiredCapacity)
		addInt32(m, "min_size", e.MinSize)
		addInt32(m, "max_size", e.MaxSize)
		addStr(m, "health_check_type", aws.ToString(e.HealthCheckType))
		m["instance_count"] = len(e.Instances)

	case sqs.Queue:
		attrs := e.GetAttributes()
		addStr(m, "messages_available", attrs["ApproximateNumberOfMessages"])
		addStr(m, "messages_in_flight", attrs["ApproximateNumberOfMessagesNotVisible"])
		addStr(m, "visibility_timeout_s", attrs["VisibilityTimeout"])
		addStr(m, "fifo", attrs["FifoQueue"])

	case sns.Topic:
		attrs := e.GetAttributes()
		addStr(m, "subscriptions_confirmed", attrs["SubscriptionsConfirmed"])
		addStr(m, "subscriptions_pending", attrs["SubscriptionsPending"])

	case cloudfront.DistributionTenantSummary:
		addStr(m, "state", aws.ToString(e.Status))
		addBool(m, "enabled", e.Enabled)
		addStr(m, "distribution_id", aws.ToString(e.DistributionId))
		addStr(m, "connection_group_id", aws.ToString(e.ConnectionGroupId))
		if domains := e.DomainNames(); len(domains) > 0 {
			m["domains"] = domains
			m["domains_active"] = e.DomainsActive()
		}
		if e.Customizations != nil {
			if e.Customizations.Certificate != nil {
				addStr(m, "certificate_arn", aws.ToString(e.Customizations.Certificate.Arn))
			}
			if e.Customizations.WebAcl != nil {
				addStr(m, "web_acl_arn", aws.ToString(e.Customizations.WebAcl.Arn))
				addStr(m, "web_acl_action", string(e.Customizations.WebAcl.Action))
			}
		}
		addTime(m, "last_modified_at", e.LastModifiedTime)

	case cloudfront.ConnectionGroup:
		addStr(m, "state", aws.ToString(e.Status))
		addBool(m, "enabled", e.Enabled)
		addBool(m, "is_default", e.IsDefault)
		addStr(m, "routing_endpoint", e.GetRoutingEndpoint())
		addStr(m, "anycast_ip_list_id", aws.ToString(e.AnycastIpListId))
		addTime(m, "last_modified_at", e.LastModifiedTime)

	default:
		// A resource type with no curated case above still gets attributes if
		// it can supply its own — which is what the Cloud Control fallback
		// resources do. Everything downstream (the state/attribute filters,
		// group_by=state and group_by=attr:<key>, and the summary view) reads
		// through this one map, so this single case makes the whole feature set
		// work for every fallback type without any per-type code.
		if provider, ok := r.(attributeProvider); ok {
			return genericAttributes(provider.GetAttributes())
		}

		return nil
	}

	if len(m) == 0 {
		return nil
	}

	return m
}

// attributeProvider is implemented by entities that carry their own untyped
// attribute bag (awslib's Cloud Control resources).
type attributeProvider interface {
	GetAttributes() map[string]any
}

// genericAttributes adapts a provider-native property bag to the curated-attribute
// conventions the rest of the server relies on: snake_case keys, so a caller
// filters on attribute=instance_type for a fallback type exactly as it would for
// a curated one, and "State"/"Status" become "state"/"status" so resourceState
// can lift them.
//
// Only top-level scalars are kept. Nested objects and arrays are deliberately
// dropped: attributes is meant to stay compact enough for the summary view, and
// viewDetail's raw field already carries the complete entity.
func genericAttributes(attrs map[string]any) map[string]any {
	if len(attrs) == 0 {
		return nil
	}

	m := make(map[string]any, len(attrs))
	for key, value := range attrs {
		// Tags are surfaced as the DTO's own tags field.
		if key == "Tags" {
			continue
		}

		switch value.(type) {
		case string, bool, float64, int, int32, int64:
			m[snakeCase(key)] = value
		}
	}

	if len(m) == 0 {
		return nil
	}

	return m
}

// snakeCase converts an AWS property name to the snake_case spelling used by the
// curated attributes ("BucketName" → "bucket_name", "DBInstanceClass" →
// "db_instance_class", "S3Bucket" → "s3_bucket", "ARN" → "arn").
func snakeCase(s string) string {
	runes := []rune(s)

	var b strings.Builder
	b.Grow(len(runes) + 4)

	for i, r := range runes {
		if i > 0 && unicode.IsUpper(r) {
			prevNotUpper := !unicode.IsUpper(runes[i-1])
			// An acronym ending: the "I" in "DBInstance" starts a new word.
			acronymEnd := i+1 < len(runes) && unicode.IsLower(runes[i+1])

			if prevNotUpper || acronymEnd {
				b.WriteRune('_')
			}
		}

		b.WriteRune(unicode.ToLower(r))
	}

	return b.String()
}

// addStr sets key to v only when v is non-empty, keeping the map compact.
func addStr(m map[string]any, key, v string) {
	if v != "" {
		m[key] = v
	}
}

// addInt32 sets key to *p only when p is non-nil.
func addInt32(m map[string]any, key string, p *int32) {
	if p != nil {
		m[key] = *p
	}
}

// addInt64 sets key to *p only when p is non-nil.
func addInt64(m map[string]any, key string, p *int64) {
	if p != nil {
		m[key] = *p
	}
}

// addBool sets key to *p only when p is non-nil.
func addBool(m map[string]any, key string, p *bool) {
	if p != nil {
		m[key] = *p
	}
}

// addTime sets key to an RFC3339 string only when p is non-nil and non-zero.
func addTime(m map[string]any, key string, p *time.Time) {
	if p != nil && !p.IsZero() {
		m[key] = p.Format(time.RFC3339)
	}
}
